package imagebake

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestRenderUnattend(t *testing.T) {
	out, err := RenderUnattend("Abc123xyz")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "{{") {
		t.Error("template residue in output")
	}
	if strings.Count(out, "<Value>Abc123xyz</Value>") != 2 {
		t.Errorf("password must appear in AdministratorPassword and AutoLogon:\n%s", out)
	}
	var doc struct {
		XMLName  xml.Name `xml:"unattend"`
		Settings []struct {
			Pass string `xml:"pass,attr"`
		} `xml:"settings"`
	}
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not well-formed XML: %v", err)
	}
	passes := map[string]bool{}
	for _, s := range doc.Settings {
		passes[s.Pass] = true
	}
	if !passes["specialize"] || !passes["oobeSystem"] {
		t.Errorf("passes = %v", passes)
	}
}

func TestRenderUnattendEscapes(t *testing.T) {
	out, err := RenderUnattend(`a<b&"c`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a&lt;b&amp;&#34;c") {
		t.Errorf("password not XML-escaped:\n%s", out)
	}
}

func TestRandomPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		p, err := RandomPassword(32)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 32 {
			t.Errorf("len = %d", len(p))
		}
		var upper, lower, digit bool
		for _, c := range p {
			switch {
			case c >= 'A' && c <= 'Z':
				upper = true
			case c >= 'a' && c <= 'z':
				lower = true
			case c >= '0' && c <= '9':
				digit = true
			default:
				t.Errorf("unexpected char %q", c)
			}
		}
		if !upper || !lower || !digit {
			t.Errorf("%q lacks a character class (Windows complexity policy)", p)
		}
		if seen[p] {
			t.Error("duplicate password")
		}
		seen[p] = true
	}
}
