package main

import "testing"

// Step names routinely contain hyphens (save-gocache, open-ticket, image-portal), and
// expr reads steps.open-ticket.status as "steps.open MINUS ticket.status". The dotted
// form must fail at COMPILE time — a 400 when the pipeline is saved — rather than
// evaluate to something surprising mid-run; the bracket form is how you address them.
func TestCompileWhen_HyphenatedStepNameNeedsBrackets(t *testing.T) {
	if _, err := compileWhen("steps.open-ticket.status == 'failed'"); err == nil {
		t.Error("a dotted hyphenated name must not compile — it parses as a subtraction")
	}
	if _, err := compileWhen(`steps["open-ticket"].status == 'failed'`); err != nil {
		t.Errorf("bracket syntax must compile: %v", err)
	}
	// A name with no hyphen reads naturally either way.
	if _, err := compileWhen("steps.build.status == 'failed'"); err != nil {
		t.Errorf("plain dotted name must compile: %v", err)
	}
}

// An empty condition is the unconditional edge — "taken iff From completed" — and must
// not reach the compiler at all.
func TestCompileWhen_EmptyIsUnconditional(t *testing.T) {
	p, err := compileWhen("")
	if err != nil {
		t.Fatalf("empty when must be valid: %v", err)
	}
	if p != nil {
		t.Error("empty when should compile to no program")
	}
}

// The point of compiling against a typed environment: a typo is a 400 when the
// pipeline is saved, not a silently-false edge at 3am.
func TestCompileWhen_RejectsNonsense(t *testing.T) {
	for _, src := range []string{
		"steps.build.nosuchfield == 1", // unknown field on a known type
		"steps.build.status",           // not a boolean
		"inputs.x +",                   // syntax error
	} {
		if _, err := compileWhen(src); err == nil {
			t.Errorf("compileWhen(%q) = nil error, want a compile failure", src)
		}
	}
}
