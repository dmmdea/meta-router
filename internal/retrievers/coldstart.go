package retrievers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

// A model router (llama-swap) unloads an idle embedder and loads it again on the next request,
// which takes seconds. A hook whose whole budget is one second that POSTs the query into a
// cold router waits out that budget, cancels, and falls back to BM25: every first prompt
// after an idle spell paid the full embed budget for nothing (F42). ProbeEmbedder asks the
// router first; WarmUpEmbedder starts the load without waiting for it.

// EmbedState is a router's answer to "is the embed model loaded?".
type EmbedState int

const (
	// EmbedUnknown means nothing says the embedder is cold: the endpoint has no /running API
	// (a plain embedding server), the router does not list the model, or nothing answered.
	// The caller embeds as it always did — a plain server has no cold start to avoid, and an
	// unlisted model is better reported by the router's own rejection than hidden by a skip.
	EmbedUnknown EmbedState = iota
	// EmbedWarm means the router reports the model loaded and ready.
	EmbedWarm
	// EmbedCold means the model is not loaded, or the router cannot be read (an embed POST
	// would block on a router that cannot answer a status query).
	EmbedCold
)

// EmbedProbe is the verdict plus what the caller needs to act on it.
type EmbedProbe struct {
	State  EmbedState
	Target string // base URL the verdict came from; a warm-up goes there
	// Warmable is set on EmbedCold when a warm-up request is worth sending: false while a load
	// is already in flight (a second request adds nothing), and false for an unreadable
	// endpoint nobody configured (nothing proved it is a model router, so it gets no traffic).
	Warmable bool
	Reason   string // one line for the usage row; never a path or prompt text
}

// ProbeEmbedder asks the router behind endpointSpec (see ResolveEndpoints) whether model is
// loaded, within budget in total. Candidates are walked in order exactly as embed() walks
// them: a candidate that refuses the connection is skipped, and the first one that answers
// decides — which is also what the embed call itself would have waited on.
func ProbeEmbedder(endpointSpec, model string, budget time.Duration) EmbedProbe {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{
		DialContext:       (&net.Dialer{Timeout: ConnectTimeout}).DialContext,
		DisableKeepAlives: true,
	}}
	for _, ep := range resolveEndpoints(endpointSpec) {
		if ctx.Err() != nil {
			break
		}
		if p, answered := probeRouter(ctx, hc, ep, model); answered {
			return p
		}
	}
	return EmbedProbe{State: EmbedUnknown, Reason: "no endpoint reachable"}
}

// probeRouter reports answered=false only when the endpoint refused the connection.
func probeRouter(ctx context.Context, hc *http.Client, ep Endpoint, model string) (EmbedProbe, bool) {
	cold := func(reason string, warmable bool) (EmbedProbe, bool) {
		return EmbedProbe{State: EmbedCold, Target: ep.URL, Warmable: warmable, Reason: reason}, true
	}
	// classify maps a router process state to a verdict. "starting" means a load is already
	// in flight: skip the wait but send no second request.
	classify := func(state string) (EmbedProbe, bool) {
		switch strings.ToLower(state) {
		case "ready":
			return EmbedProbe{State: EmbedWarm, Target: ep.URL}, true
		case "starting":
			return cold("embed model is loading", false)
		default:
			return cold("embed model state "+state, true)
		}
	}
	unknown := func(reason string) (EmbedProbe, bool) {
		return EmbedProbe{State: EmbedUnknown, Target: ep.URL, Reason: reason}, true
	}
	// A router that cannot be read is treated as cold, but only an endpoint somebody
	// configured gets the warm-up: the built-in chain's guesses are never sent traffic on
	// the strength of a failed read.
	unreadable := func(what string) (EmbedProbe, bool) {
		return cold("router unreadable ("+what+")", !ep.Unverified)
	}

	status, body, err := routerGet(ctx, hc, ep.URL+"/running")
	if err != nil {
		if isDialFailure(err) {
			return EmbedProbe{}, false
		}
		return unreadable("/running: " + shortErr(err))
	}
	if status >= 500 {
		return unreadable(fmt.Sprintf("/running: status %d", status))
	}
	running, ok := parseRunning(status, body)
	if !ok {
		return unknown("no /running API here")
	}
	if state, listed := running[model]; listed {
		return classify(state)
	}

	// Not running under the requested name, which may be an alias of a model that IS running
	// (the router reports the real id). Resolve before concluding it is cold: a false "cold"
	// would silently turn the embedder off for good. The list is only evidence when it can be
	// read; a router with no usable list cannot be asked, and /running already says "not
	// loaded", so that stands.
	realID, listed, readable := "", false, false
	if status, body, err = routerGet(ctx, hc, ep.URL+"/v1/models"); err == nil {
		realID, listed, readable = resolveModelID(status, body, model)
	}
	if readable && !listed {
		return unknown("model not listed by the router")
	}
	if state, up := running[realID]; listed && up {
		return classify(state)
	}
	return cold("embed model not loaded", true)
}

func routerGet(ctx context.Context, hc *http.Client, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

// parseRunning reads llama-swap's /running listing into model -> state. ok is false for
// anything that is not that listing (404, an HTML page, JSON without the key): such an
// endpoint does not speak the API, which is different from a router that failed to answer.
func parseRunning(status int, body []byte) (map[string]string, bool) {
	if status < 200 || status > 299 {
		return nil, false
	}
	var out struct {
		Running *[]struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if json.Unmarshal(body, &out) != nil || out.Running == nil {
		return nil, false
	}
	m := make(map[string]string, len(*out.Running))
	for _, r := range *out.Running {
		m[r.Model] = r.State
	}
	return m, true
}

// resolveModelID finds the router's real id for a requested model name, matching the id
// itself or an alias in meta.llamaswap.aliases. readable is false when the body is not a
// model list at all.
func resolveModelID(status int, body []byte, model string) (id string, listed, readable bool) {
	if status < 200 || status > 299 {
		return "", false, false
	}
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				LlamaSwap struct {
					Aliases []string `json:"aliases"`
				} `json:"llamaswap"`
			} `json:"meta"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Data == nil {
		return "", false, false
	}
	for _, d := range out.Data {
		if d.ID == model {
			return d.ID, true, true
		}
		for _, a := range d.Meta.LlamaSwap.Aliases {
			if a == model {
				return d.ID, true, true
			}
		}
	}
	return "", false, true
}

// isDialFailure is true when the connection never opened: refused, unroutable, or the dial
// itself timed out. Anything after the connect (a hang, a reset, a short body) is a router
// that accepted us and then could not answer.
func isDialFailure(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

func shortErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return "read failed"
}

// WarmUpEmbedder asks the router at target for a one-token embedding so it starts loading
// model, and returns without waiting for the load (seconds). It returns once the request is on
// the wire plus hold, or when deadline passes, then abandons the response: closing the
// connection does not stop the router's load, which is how a cancelled embed call has always
// warmed it. The input is a fixed token, never prompt text.
func WarmUpEmbedder(target, model string, hold, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	wrote := make(chan struct{})
	var once sync.Once
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { once.Do(func() { close(wrote) }) },
	})
	body, err := json.Marshal(map[string]any{"model": model, "input": []string{"warm"}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Transport: &http.Transport{
		DialContext:       (&net.Dialer{Timeout: ConnectTimeout}).DialContext,
		DisableKeepAlives: true,
	}}
	done := make(chan error, 1)
	go func() {
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-wrote:
		select {
		case <-time.After(hold):
			cancel()
			<-done
		case <-done: // answered inside the hold: it was already warm
		}
		return nil
	case err := <-done:
		return err
	}
}
