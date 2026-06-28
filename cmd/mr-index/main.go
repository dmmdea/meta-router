// Command mr-index builds and refreshes the on-disk skill index that mr-hook
// loads. Build embeds all skills; refresh re-embeds only changed ones.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/index"
)

type config struct {
	cmd        string
	skillRoots string
	endpoint   string
	out        string
}

func parseArgs(argv []string) (config, error) {
	if len(argv) == 0 {
		return config{}, fmt.Errorf("usage: mr-index <build|refresh> [flags]")
	}
	cmd := argv[0]
	if cmd != "build" && cmd != "refresh" {
		return config{}, fmt.Errorf("unknown subcommand %q (want build|refresh)", cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	skillRoots := fs.String("skill-roots", "", "comma-separated skill root dirs (default ~/.claude/skills)")
	endpoint := fs.String("endpoint", "http://127.0.0.1:11436", "embedder endpoint")
	out := fs.String("out", "", "index path (default ~/.meta-router/index.json)")
	if err := fs.Parse(argv[1:]); err != nil {
		return config{}, err
	}
	return config{cmd: cmd, skillRoots: *skillRoots, endpoint: *endpoint, out: *out}, nil
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if cfg.skillRoots == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			fmt.Fprintf(os.Stderr, "cannot resolve home dir: %v\n", herr)
			os.Exit(1)
		}
		cfg.skillRoots = filepath.Join(home, ".claude", "skills")
	}
	outPath := cfg.out
	if outPath == "" {
		outPath, err = index.DefaultIndexPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	roots := strings.Split(cfg.skillRoots, ",")
	for i := range roots {
		roots[i] = strings.TrimSpace(roots[i])
	}

	switch cfg.cmd {
	case "build":
		raw, err := catalog.Harvest(roots)
		if err != nil {
			fmt.Fprintf(os.Stderr, "harvest: %v\n", err)
			os.Exit(1)
		}
		skills := catalog.DedupByID(raw)
		idx, err := index.Build(skills, cfg.endpoint, 60*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "build: %v\n", err)
			os.Exit(1)
		}
		if err := idx.Save(outPath); err != nil {
			fmt.Fprintf(os.Stderr, "save: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("built %d skills (dim %d) → %s\n", len(idx.Entries), idx.Dim, outPath)
	case "refresh":
		idx, err := index.Load(outPath)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "load: %v\n", err)
				os.Exit(1)
			}
			// no index yet → build fresh
			raw, herr := catalog.Harvest(roots)
			if herr != nil {
				fmt.Fprintf(os.Stderr, "harvest: %v\n", herr)
				os.Exit(1)
			}
			idx, err = index.Build(catalog.DedupByID(raw), cfg.endpoint, 60*time.Second)
			if err != nil {
				fmt.Fprintf(os.Stderr, "build: %v\n", err)
				os.Exit(1)
			}
			if err := idx.Save(outPath); err != nil {
				fmt.Fprintf(os.Stderr, "save: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("no prior index; built %d skills → %s\n", len(idx.Entries), outPath)
			return
		}
		added, updated, removed, err := idx.Refresh(roots, cfg.endpoint, 60*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "refresh: %v\n", err)
			os.Exit(1)
		}
		if err := idx.Save(outPath); err != nil {
			fmt.Fprintf(os.Stderr, "save: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("refreshed: +%d ~%d -%d (now %d skills) → %s\n", added, updated, removed, len(idx.Entries), outPath)
	}
}
