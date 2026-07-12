//go:build ignore

// Offline scorer for the PR B step-1 gate.
//
// Reads the (reply, facts) pairs captured by COMPSHARE_GROUNDING_DUMP and runs
// the REAL grounding.Check over them — not a reimplementation. A scorer that
// re-derived the rules in Python would measure a validator that does not exist,
// and the number it produced would not transfer to the runtime.
//
//	go run grounding_score.go -dump out/_g_cand_dump.jsonl -label cand
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/compshare-agent/internal/grounding"
)

type rec struct {
	Turn      int      `json:"turn"`
	UserText  string   `json:"user_text"`
	Reply     string   `json:"reply"`
	FactCount int      `json:"fact_count"`
	Numbers   []string `json:"numbers"`
	Text      []string `json:"text"`
}

func main() {
	dump := flag.String("dump", "", "grounding dump jsonl")
	label := flag.String("label", "", "arm label")
	show := flag.Int("show", 25, "how many flagged turns to print")
	flag.Parse()

	f, err := os.Open(*dump)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer f.Close()

	var (
		total, withFacts, noFacts, flagged int
		violTotal                          int
		byClaim                            = map[string]int{}
		examples                           []string
	)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)

	// Facts accumulate across the SESSION, not the turn.
	//
	// Turn-scoping was the first design and it was wrong. On a real captured session the
	// user uploaded a screenshot of their instance on turn 1; by turn 3 the model was
	// still correctly referring to that instance by ID, and a turn-scoped validator
	// called the ID invented — because no tool had returned it *that turn*. It punished
	// the model for carrying context forward, which is the single behaviour this whole
	// change exists to buy. "The model did not invent this" is a claim about the
	// conversation, not about one turn. (Quoting a fact that has since gone STALE is a
	// different bug and not this guard's job.)
	//
	// The engine numbers turns from 1 per session, so turn==1 opens a new session.
	facts := grounding.NewFacts()
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		total++
		if r.Turn <= 1 {
			facts = grounding.NewFacts()
		}

		// Rebuild the fact set with the SAME helpers the engine calls, so this measures
		// the shipping validator and not a lookalike.
		for _, n := range r.Numbers {
			facts.AddRaw(n)
		}
		for _, t := range r.Text {
			facts.AddRaw(t)
		}
		facts.AddScreenshotEvidence(r.UserText)
		facts.AddUserReferents(r.UserText)

		if facts.Empty() {
			noFacts++
			continue
		}
		withFacts++

		v := grounding.Check(r.Reply, facts)
		if len(v) == 0 {
			continue
		}
		flagged++
		violTotal += len(v)
		var cs []string
		for _, x := range v {
			byClaim[x.Claim]++
			cs = append(cs, x.String())
		}
		if len(examples) < *show {
			examples = append(examples, fmt.Sprintf("  t%-2d %-28q -> %s",
				r.Turn, trunc(r.UserText, 26), strings.Join(cs, " ")))
		}
	}

	fmt.Printf("=========== %s ===========\n", *label)
	fmt.Printf("turns captured          %4d\n", total)
	fmt.Printf("  no tool ran (skipped) %4d   <- validator inert, cannot judge\n", noFacts)
	fmt.Printf("  validator active      %4d\n", withFacts)
	fmt.Printf("  ...of which FLAGGED   %4d   (%.1f%% of active)\n",
		flagged, 100*float64(flagged)/float64(max(withFacts, 1)))
	fmt.Printf("total violations        %4d\n\n", violTotal)

	type kv struct {
		k string
		n int
	}
	var ks []kv
	for k, n := range byClaim {
		ks = append(ks, kv{k, n})
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].n > ks[j].n })
	fmt.Println("most-flagged claims:")
	for i, x := range ks {
		if i >= 20 {
			break
		}
		fmt.Printf("  %3dx  %s\n", x.n, x.k)
	}
	fmt.Println("\nflagged turns:")
	for _, e := range examples {
		fmt.Println(e)
	}
}

func trunc(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
