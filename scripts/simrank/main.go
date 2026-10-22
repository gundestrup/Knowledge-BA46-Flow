// Command simrank runs an ad-hoc ranking analysis over scanned document
// signatures in a state.db — a diagnostics helper, not part of the app.
//
// Usage:
//
//	go run ./scripts/simrank <state.db>
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/similarity"
)

type pair struct {
	a, b string
	sim  float64
}

// probes are corpus documents whose nearest neighbours are worth eyeballing —
// absent paths are skipped, so the list survives fixture pruning.
var probes = []string{
	"languages/jekyll.md", "languages/ruby.md",
	"frameworks/jekyll/README.md", "repos/MediCalc.md",
	"patterns/docker-multiarch.md",
	"lessons-learned/semgrep-metavars-match-quoted-args.md",
}

func run(dbPath string, out io.Writer) error {
	db, err := database.Open(context.Background(), dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	docs, err := db.Documents(context.Background())
	if err != nil {
		return err
	}
	sigs := map[string][]uint64{}
	var paths []string
	for _, d := range docs {
		if s := similarity.Unmarshal(d.Signature); len(s) > 0 {
			sigs[d.Path] = s
			paths = append(paths, d.Path)
		}
	}
	sort.Strings(paths)
	fmt.Fprintf(out, "docs with signatures: %d\n", len(paths))

	var pairs []pair
	buckets := map[string]int{"ge50": 0, "ge30": 0, "ge10": 0, "ge5": 0, "zero": 0}
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			s := similarity.Estimate(sigs[paths[i]], sigs[paths[j]])
			switch {
			case s >= 0.5:
				buckets["ge50"]++
			case s >= 0.3:
				buckets["ge30"]++
			case s >= 0.1:
				buckets["ge10"]++
			case s >= 0.05:
				buckets["ge5"]++
			case s == 0:
				buckets["zero"]++
			}
			if s > 0 {
				pairs = append(pairs, pair{paths[i], paths[j], s})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].sim > pairs[j].sim })
	total := len(paths) * (len(paths) - 1) / 2
	fmt.Fprintf(out, "pairs: %d | >=50%%: %d | 30-50%%: %d | 10-30%%: %d | 5-10%%: %d | 0%%: %d\n\n",
		total, buckets["ge50"], buckets["ge30"], buckets["ge10"], buckets["ge5"], buckets["zero"])

	n := min(30, len(pairs))
	fmt.Fprintln(out, "=== top pairs ===")
	for _, p := range pairs[:n] {
		fmt.Fprintf(out, "%5.1f%%  %s  <->  %s\n", p.sim*100, p.a, p.b)
	}

	for _, probe := range probes {
		if _, ok := sigs[probe]; !ok {
			continue
		}
		var nn []pair
		for _, q := range paths {
			if q == probe {
				continue
			}
			nn = append(nn, pair{probe, q, similarity.Estimate(sigs[probe], sigs[q])})
		}
		sort.Slice(nn, func(i, j int) bool { return nn[i].sim > nn[j].sim })
		fmt.Fprintf(out, "\n=== nearest to %s ===\n", probe)
		for _, p := range nn[:min(5, len(nn))] {
			fmt.Fprintf(out, "%5.1f%%  %s\n", p.sim*100, p.b)
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: simrank <state.db>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "simrank:", err)
		os.Exit(1)
	}
}
