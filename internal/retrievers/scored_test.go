package retrievers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"github.com/dmmdea/meta-router/internal/catalog"
)

func TestNewHybridFromIndex_CountMismatch(t *testing.T) {
	_, err := NewHybridFromIndex(
		[]catalog.Skill{{ID: "skills:a"}},
		[][]float64{}, // 0 vecs for 1 skill
		"http://127.0.0.1:11436", time.Second)
	if err == nil {
		t.Fatal("expected error on skills/vecs length mismatch")
	}
}

func TestEmbedFromVectors_RankByCosine_NoNetwork(t *testing.T) {
	// rankByCosine needs the query embedded, which hits the network — so this
	// test only checks construction + that Retrieve degrades to an error (not a
	// panic) when the endpoint is bogus.
	e := NewEmbedFromVectors([]string{"skills:a"}, [][]float64{{1, 0}}, "http://127.0.0.1:1", 200*time.Millisecond)
	if _, err := e.Retrieve("anything", 1); err == nil {
		t.Fatal("expected error from unreachable endpoint")
	}
}

func TestRankByCosine_HTTPStub_RanksByCosine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
	}))
	defer srv.Close()
	e := NewEmbedFromVectors([]string{"skills:a", "skills:b"}, [][]float64{{1, 0}, {0, 1}}, srv.URL, time.Second)
	ids, err := e.Retrieve("q", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "skills:a" {
		t.Fatalf("got %v want [skills:a]", ids)
	}
}

func TestRankByCosine_EmptyResponseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	e := NewEmbedFromVectors([]string{"skills:a"}, [][]float64{{1, 0}}, srv.URL, time.Second)
	if _, err := e.Retrieve("q", 1); err == nil {
		t.Fatal("expected error when embed response is empty")
	}
}
