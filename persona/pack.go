package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"go.vicoworks.com/open-persona/graph"
)

// A pack is one SQLite file, distributed with the extension .persona. Its
// header carries PackApplicationID (SQLite's application_id) and the format
// version (user_version), so a pack identifies itself without being opened
// as a graph, and a newer format is refused instead of misread.
const (
	PackExt                 = ".persona"
	PackApplicationID int32 = 0x4F504E50 // "OPNP"
	PackFormat        int32 = 1
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// CheckID reports whether id is a valid persona ID: 1-63 lowercase letters,
// digits and hyphens, starting with a letter or digit.
func CheckID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("persona: invalid id %q (use lowercase letters, digits and hyphens)", id)
	}
	return nil
}

// CheckVersion reports whether v is a full semantic version without a "v"
// prefix, such as "1.0.0" or "1.1.0-rc.1".
func CheckVersion(v string) error {
	if strings.HasPrefix(v, "v") || semver.Canonical("v"+v) != "v"+v {
		return fmt.Errorf("persona: invalid version %q (use semantic versioning, e.g. 1.0.0)", v)
	}
	return nil
}

// CompareVersions orders two valid versions by semantic-versioning rules:
// -1 if a < b, 0 if equal, +1 if a > b.
func CompareVersions(a, b string) int { return semver.Compare("v"+a, "v"+b) }

// Slug turns a name into an ID: "Fina Finance" -> "fina-finance".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// normalize fills defaults and validates the spec before training.
func (s *Spec) normalize() error {
	s.Name, s.Role = strings.TrimSpace(s.Name), strings.TrimSpace(s.Role)
	if s.Name == "" || s.Role == "" {
		return errors.New("persona: spec needs name and role")
	}
	if s.ID == "" {
		s.ID = Slug(s.Name)
	}
	if s.Version == "" {
		s.Version = "0.1.0"
	}
	return errors.Join(CheckID(s.ID), CheckVersion(s.Version))
}

// Manifest describes a pack: everything except the knowledge itself.
type Manifest struct {
	Format     int32       `json:"format"` // 0 = written before the format was stamped
	Spec       Spec        `json:"spec"`
	Core       string      `json:"core"`
	Report     *Report     `json:"report,omitempty"`
	Eval       *EvalResult `json:"eval,omitempty"`
	TrainedAt  time.Time   `json:"trained_at"`
	TrainModel string      `json:"train_model,omitempty"`
	EmbedModel string      `json:"embed_model,omitempty"`
}

// Manifest reads the pack's metadata.
func (p *Persona) Manifest(ctx context.Context) (*Manifest, error) {
	m := &Manifest{Format: p.format, Spec: p.Spec, Core: p.Core, EmbedModel: p.EmbedModel}
	get := func(k string) string { v, _ := p.store.GetMeta(ctx, k); return v }
	if v := get("report"); v != "" {
		m.Report = new(Report)
		if err := json.Unmarshal([]byte(v), m.Report); err != nil {
			return nil, fmt.Errorf("persona: bad report: %w", err)
		}
	}
	if v := get("eval"); v != "" {
		m.Eval = new(EvalResult)
		if err := json.Unmarshal([]byte(v), m.Eval); err != nil {
			return nil, fmt.Errorf("persona: bad eval: %w", err)
		}
	}
	m.TrainedAt, _ = time.Parse(time.RFC3339, get("trained_at"))
	m.TrainModel = get("train_model")
	return m, nil
}

// Unit is one compiled knowledge unit of a pack.
type Unit struct {
	ID    int64    `json:"id"`
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags,omitempty"`
}

// Units lists every compiled knowledge unit with its tags, for review,
// diffing between versions, or indexing in a catalog.
func (p *Persona) Units(ctx context.Context) ([]Unit, error) {
	var out []Unit
	for _, kind := range UnitKinds {
		nodes, err := p.store.List(ctx, scope, kind, 1<<30)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			out = append(out, Unit{ID: n.ID, Kind: string(n.Kind), Title: n.Title, Body: n.Body})
		}
	}
	index := make(map[int64]int, len(out))
	ids := make([]int64, len(out))
	for i, u := range out {
		index[u.ID], ids[i] = i, u.ID
	}
	for start := 0; start < len(ids); start += 500 {
		links, err := p.store.Outgoing(ctx, ids[start:min(start+500, len(ids))], graph.RelMentions, 50)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			u := &out[index[l.From]]
			u.Tags = append(u.Tags, l.Node.Title)
		}
	}
	return out, nil
}

// KnowledgeToolDescription describes this persona's knowledge search to an
// agent's model, for a tool definition (MCP, OpenAI or Anthropic tools).
func (s Spec) KnowledgeToolDescription() string {
	d := fmt.Sprintf("Search the working knowledge of %s (%s).", s.Name, s.Role)
	if len(s.Expertise) > 0 {
		d += " Covers: " + strings.Join(s.Expertise, "; ") + "."
	}
	return d + " Ask one focused question per call (one place, one rule, one route); call it several times for a multi-part task. Returns knowledge units, not an answer."
}
