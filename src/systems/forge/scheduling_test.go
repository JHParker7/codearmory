package main

import "testing"

func TestParseNodeSelector(t *testing.T) {
	got := parseNodeSelector("forge-runtime=kata, gpu=true ")
	if got["forge-runtime"] != "kata" || got["gpu"] != "true" {
		t.Errorf("parseNodeSelector = %v", got)
	}
	if parseNodeSelector("") != nil || parseNodeSelector("  ") != nil {
		t.Error("blank must yield nil")
	}
	if parseNodeSelector("bogus,=noval") != nil {
		t.Error("no valid pairs must yield nil")
	}
}

func TestParseTolerations(t *testing.T) {
	got := parseTolerations("sandbox, gpu ")
	if len(got) != 2 || got[0].Key != "sandbox" || got[0].Operator != "Exists" {
		t.Errorf("parseTolerations = %+v", got)
	}
	if parseTolerations("") != nil {
		t.Error("blank must yield nil")
	}
}
