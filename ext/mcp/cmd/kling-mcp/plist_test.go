package main

import (
	"encoding/xml"
	"reflect"
	"strings"
	"testing"
)

// El plist del LaunchAgent lleva un <string> por argumento y escapado: un
// argumento con espacios sigue siendo uno, y un & o un < no rompen el XML.
func TestLaunchAgentPlist(t *testing.T) {
	argv := []string{"/usr/local/bin/kling-bridge", "-listen", "127.0.0.1:9100", "--",
		"/opt/bin/srv", "--name=a b", "q=1&r=<2>", `"comillas"`}
	body := launchAgentPlist("/Users/a&b", argv)

	var doc struct {
		Dict struct {
			Items []struct {
				XMLName xml.Name
				Strings []string `xml:"string"`
				Chars   string   `xml:",chardata"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	d := xml.NewDecoder(strings.NewReader(body))
	d.Strict = true
	d.Entity = xml.HTMLEntity
	if err := d.Decode(&doc); err != nil {
		t.Fatalf("the plist is not valid XML: %v\n%s", err, body)
	}
	var got []string
	for i, it := range doc.Dict.Items {
		if it.XMLName.Local == "key" && strings.TrimSpace(it.Chars) == "ProgramArguments" {
			got = doc.Dict.Items[i+1].Strings
		}
	}
	if !reflect.DeepEqual(got, argv) {
		t.Fatalf("ProgramArguments = %q, want %q", got, argv)
	}
	if !strings.Contains(body, "/Users/a&amp;b/Library/Logs") {
		t.Error("home is not escaped")
	}
}

// -cmd se parte respetando comillas (strings.Fields no admitía argumentos con espacios).
func TestSplitArgs(t *testing.T) {
	for in, want := range map[string][]string{
		"engram mcp --tools=agent":    {"engram", "mcp", "--tools=agent"},
		`srv --name "a b" 'c d' e\ f`: {"srv", "--name", "a b", "c d", "e f"},
		`srv "x \"y\"" ''`:            {"srv", `x "y"`, ""},
		"  srv\t-v  ":                 {"srv", "-v"},
	} {
		got, err := splitArgs(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitArgs(`srv "abierta`); err == nil {
		t.Error("an unterminated quote was accepted")
	}
}
