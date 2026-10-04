package main

import (
	"strings"
	"testing"
)

func TestParseFreehandCallGraphCanonical(t *testing.T) {
	raw := "# soroq.callgraph.v1\n" +
		"table\tpackage:a/a.dart||f\tpackage:a/a.dart|S|m\n" +
		"direct\tpackage:a/a.dart||g\tpackage:a/a.dart||f\n" +
		"direct\tpackage:a/a.dart||g\tpackage:a/a.dart||f\n" +
		"ic\tpackage:a/a.dart||g\t?|?|dyn:greet\n" +
		"# written=4\n"
	edges, err := parseFreehandCallGraph([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 3 {
		t.Fatalf("want 3 distinct edges, got %d: %v", len(edges), edges)
	}
	canon := renderFreehandCallGraph(edges)
	again, err := parseFreehandCallGraph(canon)
	if err != nil {
		t.Fatal(err)
	}
	if string(renderFreehandCallGraph(again)) != string(canon) {
		t.Fatal("canonical form is not a fixed point")
	}
	if !strings.HasSuffix(string(canon), "# written=3\n") {
		t.Fatalf("canonical trailer: %q", canon)
	}
}

func TestParseFreehandCallGraphRefusesTruncatedOrMalformed(t *testing.T) {
	for name, raw := range map[string]string{
		"no header":    "direct\ta||f\ta||g\n# written=1\n",
		"no trailer":   "# soroq.callgraph.v1\ndirect\ta|x|f\ta|x|g\n",
		"short count":  "# soroq.callgraph.v1\ndirect\ta|x|f\ta|x|g\ndirect\ta|x|g\ta|x|h\n# written=1\n",
		"unknown kind": "# soroq.callgraph.v1\njump\ta|x|f\ta|x|g\n# written=1\n",
		"bad identity": "# soroq.callgraph.v1\ndirect\tf\ta|x|g\n# written=1\n",
		"too few cols": "# soroq.callgraph.v1\ndirect\ta|x|f\n# written=1\n",
	} {
		if _, err := parseFreehandCallGraph([]byte(raw)); err == nil {
			t.Errorf("%s: accepted %q", name, raw)
		}
	}
}

func TestValidateFreehandSwapEntry(t *testing.T) {
	defer resetFreehandR12()
	resetFreehandR12()
	e := freehandABIEntryView{BaseIdentity: "package:flutter/fw.dart::Greeter::greet", Kind: "swap:instance-member"}
	if err := validateFreehandSwapEntry(e); err == nil {
		t.Fatal("a swap must be refused when the plan did not run")
	}
	freehandR12.Active, freehandR12.EntrySwap = true, true
	freehandR12.Swaps = map[string]bool{e.BaseIdentity: true}
	if err := validateFreehandSwapEntry(e); err != nil {
		t.Fatalf("a planned swap was refused: %v", err)
	}
	if err := validateFreehandSwapEntry(freehandABIEntryView{BaseIdentity: "package:flutter/fw.dart::X::y", Kind: "swap:function"}); err == nil {
		t.Fatal("a swap the plan did not name was accepted")
	}
	if err := validateFreehandSwapEntry(freehandABIEntryView{BaseIdentity: e.BaseIdentity, Kind: "swap:constructor"}); err == nil {
		t.Fatal("an unknown swap kind was accepted")
	}
	freehandR12.EntrySwap = false
	if err := validateFreehandSwapEntry(e); err == nil {
		t.Fatal("a swap was accepted for a base whose engine cannot swap")
	}
}
