package execution

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MCPSelection freezes a session's explicit packages and scalar settings. Persistence retains
// only identities. Package bytes and setting values belong to the process lifetime.
type MCPSelection struct {
	Bindings []MCPBinding
	Packages []MCPPackage `json:"-"`
}

// MCPTarget is a single committed generation. Configured bindings and package bytes
// are transient. Removed or incomplete members still occupy their authorized slots.
type MCPTarget struct {
	Generation  int64
	Configured  MCPSelection `json:"-"`
	Unavailable map[string]string
}

// MCPBinding is an exact MCP identity. WireSettings is constructed only at the Node boundary.
type MCPBinding struct {
	PluginID      string `json:"plugin_id"`
	PluginVersion string `json:"plugin_version"`
	// Revision identifies the tenant settings read with this package, without retaining values.
	Revision int64          `json:"configuration_revision,omitempty"`
	Settings map[string]any `json:"-"`
}

// MaxMCPCounter is the largest integer shared exactly by Node and JSON clients.
const MaxMCPCounter int64 = 1<<53 - 1

// SameMembers compares the immutable authorization set, independently of the versions
// or settings a later resolution observed. Missing metadata authorizes no members.
func (s *MCPSelection) SameMembers(other *MCPSelection) bool {
	if s == nil || other == nil {
		return (s == nil || len(s.Bindings) == 0) && (other == nil || len(other.Bindings) == 0)
	}
	return slices.EqualFunc(s.Bindings, other.Bindings, func(a, b MCPBinding) bool {
		return a.PluginID == b.PluginID
	})
}

// MCPPackage contains verified package bytes, never downloaded using session credentials.
type MCPPackage struct {
	PluginID, PluginVersion, Digest, Target string
	Archive                                 []byte `json:"-"`
}

func (s MCPSelection) String() string {
	return fmt.Sprintf("MCPSelection{%d bindings, settings redacted}", len(s.Bindings))
}
func (b MCPBinding) String() string {
	return b.PluginID + "@" + b.PluginVersion + " [settings redacted]"
}

func (s MCPSelection) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, s.String()) }
func (b MCPBinding) Format(f fmt.State, _ rune)   { _, _ = fmt.Fprint(f, b.String()) }

// MCPFailure carries only stable codes and identifiers, suitable for retained diagnostics.
type MCPFailure struct {
	Code      string `json:"code"`
	PluginID  string `json:"plugin_id,omitempty"`
	SettingID string `json:"setting_id,omitempty"`
	Transport string `json:"transport,omitempty"`
}

func (f *MCPFailure) Error() string { return f.Code }

// IsMCPFailureCode accepts only the fixed setup failures exposed by Controller and Node.
func IsMCPFailureCode(code string) bool {
	switch code {
	case "mcp_plugin_missing", "mcp_setting_missing", "mcp_http_capability_missing", "mcp_load_session_missing", "mcp_node_capability_missing", "mcp_lifecycle_not_ready", "mcp_selection_invalid":
		return true
	}
	return false
}

var mcpIDPart = regexp.MustCompile(`^[a-z0-9.-]+$`)
var mcpVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// ValidMCPPluginID mirrors ora-domain PluginId, including its namespace byte bound.
func ValidMCPPluginID(id string) bool {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || len(parts[0]) > 33 {
		return false
	}
	for _, p := range parts {
		if p == "." || p == ".." || !mcpIDPart.MatchString(p) {
			return false
		}
	}
	return true
}

// ValidMCPVersion accepts the canonical spelling accepted by semver::Version.
func ValidMCPVersion(version string) bool {
	parts := mcpVersion.FindStringSubmatch(version)
	if parts == nil {
		return false
	}
	for _, p := range parts[1:4] {
		if _, err := strconv.ParseUint(p, 10, 64); err != nil {
			return false
		}
	}
	for _, p := range strings.Split(parts[4], ".") {
		if len(p) > 1 && p[0] == '0' && strings.Trim(p, "0123456789") == "" {
			return false
		}
	}
	return true
}

// Clone owns all mutable inputs before admission, preventing post-validation mutation.
func (s *MCPSelection) Clone() *MCPSelection {
	if s == nil {
		return nil
	}
	owned := &MCPSelection{Bindings: slices.Clone(s.Bindings), Packages: slices.Clone(s.Packages)}
	for i := range owned.Bindings {
		owned.Bindings[i].Settings = maps.Clone(s.Bindings[i].Settings)
	}
	for i := range owned.Packages {
		owned.Packages[i].Archive = bytes.Clone(s.Packages[i].Archive)
	}
	return owned
}

// Validate mirrors the Node protocol before any frame can be sent. It deliberately rejects
// duplicate or unsorted identities instead of changing the caller's explicit selection.
func (s *MCPSelection) Validate() error {
	if s == nil {
		return nil
	}
	invalid := &MCPFailure{Code: "mcp_selection_invalid"}
	if len(s.Bindings) > 16 {
		return invalid
	}
	total, previous := 0, ""
	for _, b := range s.Bindings {
		if !ValidMCPPluginID(b.PluginID) || previous >= b.PluginID || !ValidMCPVersion(b.PluginVersion) || b.Settings == nil || b.Revision < 0 || b.Revision > MaxMCPCounter {
			return invalid
		}
		previous = b.PluginID
		unicodeEscapes, numberAdjustment := 0, 0
		for key, value := range b.Settings {
			unicodeEscapes += strings.Count(key, "\u2028") + strings.Count(key, "\u2029")
			if !utf8.ValidString(key) {
				return invalid
			}
			switch v := value.(type) {
			case string:
				unicodeEscapes += strings.Count(v, "\u2028") + strings.Count(v, "\u2029")
				if !utf8.ValidString(v) {
					return invalid
				}
			case bool, int, int32, int64, uint, uint32, uint64:
			case float64:
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return invalid
				}
			case json.Number:
				if _, err := v.Float64(); err != nil {
					return invalid
				}
			default:
				return invalid
			}
			if _, text := value.(string); !text {
				if _, boolean := value.(bool); !boolean {
					wire, err := json.Marshal(value)
					if err != nil {
						return invalid
					}
					size, err := mcpNumberSize(string(wire))
					if err != nil {
						return invalid
					}
					numberAdjustment += size - len(wire)
				}
			}
		}
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(b.Settings) != nil {
			return invalid
		}
		// serde_json writes these UTF-8 characters literally; encoding/json escapes them.
		compact := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
		total += len(compact) - 3*unicodeEscapes + numberAdjustment
		if total > 64*1024 {
			return invalid
		}
	}
	return nil
}

// serde_json retains integer tokens that fit i64/u64. Other numbers become f64 and use
// shortest decimal formatting with scientific notation below 1e-5 or at/above 1e16.
// Count that normalized value, since Node validates after deserializing the control frame.
func mcpNumberSize(wire string) (int, error) {
	if !strings.ContainsAny(wire, ".eE") && wire != "-0" {
		if _, err := strconv.ParseInt(wire, 10, 64); err == nil {
			return len(wire), nil
		}
		if _, err := strconv.ParseUint(wire, 10, 64); err == nil {
			return len(wire), nil
		}
	}
	value, err := strconv.ParseFloat(wire, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, &MCPFailure{Code: "mcp_selection_invalid"}
	}
	abs := math.Abs(value)
	if abs != 0 && (abs < 1e-5 || abs >= 1e16) {
		raw := strconv.FormatFloat(value, 'e', -1, 64)
		mantissa, exponent, _ := strings.Cut(raw, "e")
		exp, _ := strconv.Atoi(exponent)
		return len(mantissa) + 1 + len(fmt.Sprintf("%+d", exp)), nil
	}
	raw := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(raw, ".") {
		raw += ".0"
	}
	return len(raw), nil
}
