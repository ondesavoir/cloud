package execution

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestMCPByteLimitUsesNodeNormalizedNumbers(t *testing.T) {
	for wire, want := range map[string]int{"-0": 4, "0.000001": 4, "1.0": 3, "1e+15": 18, "1e+16": 5, "100000000000000000000": 5, "1e-9": 4, "0.00001": 7} {
		got, err := mcpNumberSize(wire)
		if err != nil || got != want {
			t.Fatalf("%s size = %d, %v; want %d", wire, got, err, want)
		}
	}
	s := testMCP()
	s.Bindings[0].Settings = map[string]any{"n": math.Copysign(0, -1), "x": strings.Repeat("a", 65519)}
	if s.Validate() != nil {
		t.Fatal("exact normalized limit rejected")
	}
	s.Bindings[0].Settings["x"] = strings.Repeat("a", 65520)
	if s.Validate() == nil {
		t.Fatal("negative zero normalization escaped byte limit")
	}
}

func testMCP() *MCPSelection {
	return &MCPSelection{Bindings: []MCPBinding{{PluginID: "official/acme.tools", PluginVersion: "1.2.3", Settings: map[string]any{}}}}
}

func TestMCPAuthorizationSurvivesOnlyChangesWithinSelectedIDs(t *testing.T) {
	initial := testMCP()
	current := initial.Clone()
	current.Bindings[0].Revision++
	current.Bindings[0].PluginVersion = "2.0.0"
	current.Bindings[0].Settings = nil // a retained identity has no credential values
	if !initial.SameMembers(current) {
		t.Fatal("a package update changed the session's authorization")
	}
	current.Bindings[0].PluginID = "official/other"
	empty := &MCPSelection{}
	if initial.SameMembers(current) || initial.SameMembers(nil) || empty.SameMembers(initial) || !empty.SameMembers(nil) {
		t.Fatal("selection loss or substitution changed the authorized set")
	}
}

func TestMCPSelectionMirrorsProtocol(t *testing.T) {
	for name, change := range map[string]func(*MCPSelection){
		"nil settings":    func(s *MCPSelection) { s.Bindings[0].Settings = nil },
		"namespace bound": func(s *MCPSelection) { s.Bindings[0].PluginID = strings.Repeat("a", 34) + "/tool" },
		"uppercase":       func(s *MCPSelection) { s.Bindings[0].PluginID = "Official/tools" },
		"relative":        func(s *MCPSelection) { s.Bindings[0].PluginID = "official/.." },
		"extra slash":     func(s *MCPSelection) { s.Bindings[0].PluginID = "official/acme/tools" },
		"range":           func(s *MCPSelection) { s.Bindings[0].PluginVersion = "^1.2.3" },
		"leading zero":    func(s *MCPSelection) { s.Bindings[0].PluginVersion = "1.2.3-01" },
		"u64 overflow":    func(s *MCPSelection) { s.Bindings[0].PluginVersion = "18446744073709551616.0.0" },
		"duplicate":       func(s *MCPSelection) { s.Bindings = append(s.Bindings, s.Bindings[0]) },
		"unsorted": func(s *MCPSelection) {
			s.Bindings = append(s.Bindings, MCPBinding{PluginID: "local/a", PluginVersion: "1.0.0", Settings: map[string]any{}})
		},
		"nested":    func(s *MCPSelection) { s.Bindings[0].Settings["secret"] = map[string]any{"x": "secret"} },
		"null":      func(s *MCPSelection) { s.Bindings[0].Settings["secret"] = nil },
		"oversized": func(s *MCPSelection) { s.Bindings[0].Settings["x"] = strings.Repeat("a", 65529) },
	} {
		t.Run(name, func(t *testing.T) {
			s := testMCP()
			change(s)
			if s.Validate() == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	s := testMCP()
	s.Bindings[0].PluginVersion = "1.2.3-alpha.1+build.01"
	s.Bindings[0].Settings["x"] = strings.Repeat("a", 65528)
	if err := s.Validate(); err != nil {
		t.Fatal("exact limit rejected", err)
	}
	s.Bindings[0].Settings["x"] = strings.Repeat("\u2028", 21842) + "aa"
	if err := s.Validate(); err != nil {
		t.Fatal("UTF-8 limit rejected", err)
	}
	s.Bindings = nil
	for i := 0; i < 17; i++ {
		s.Bindings = append(s.Bindings, MCPBinding{PluginID: fmt.Sprintf("official/tool%02d", i), PluginVersion: "1.0.0", Settings: map[string]any{}})
	}
	if s.Validate() == nil {
		t.Fatal("17 servers accepted")
	}
	s.Bindings = s.Bindings[:16]
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPSettingsStayOutOfPersistenceAndDebug(t *testing.T) {
	s := testMCP()
	s.Bindings[0].Settings["token"] = "secret-marker"
	s.Packages = []MCPPackage{{Archive: []byte("archive-marker")}}
	data, err := json.Marshal(Session{MCP: s})
	if err != nil {
		t.Fatal(err)
	}
	debug := fmt.Sprintf("%+v %#v %+v %#v", s, s, s.Bindings[0], s.Bindings[0])
	for _, value := range []string{"secret-marker", "archive-marker"} {
		if strings.Contains(string(data), value) || strings.Contains(debug, value) {
			t.Fatal("runtime input leaked")
		}
	}
	owned := s.Clone()
	s.Bindings[0].Settings["token"] = "changed"
	s.Packages[0].Archive[0] = 'X'
	if owned.Bindings[0].Settings["token"] != "secret-marker" || string(owned.Packages[0].Archive) != "archive-marker" {
		t.Fatal("clone aliases caller state")
	}
}
