package persona

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.vicoworks.com/open-persona/graph"
)

func TestPackIsStampedAndDescribesItself(t *testing.T) {
	ctx := context.Background()
	evalSet := []EvalCase{
		{Q: "Who approves a 7 million rupiah expense?", Expect: []string{"CFO approval", "Rp 5,000,000"}},
		{Q: "month-end close", Expect: []string{"working day 5", "never in the policy|also absent"}},
	}
	path := trainFakeWith(t, Trainer{Eval: evalSet, TrainModel: "fake:model"}, Spec{})

	// The header survives Freeze (VACUUM), so the file identifies itself.
	store, err := graph.Open(path, graph.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	appID, format, err := store.FileFormat(ctx)
	store.Close()
	if err != nil || appID != PackApplicationID || format != PackFormat {
		t.Fatalf("header: app id %#x format %d err %v", appID, format, err)
	}

	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	m, err := p.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != PackFormat || m.Spec.ID != "fina" || m.Spec.Version != "0.1.0" || m.TrainModel != "fake:model" || m.TrainedAt.IsZero() {
		t.Errorf("manifest: %+v", m)
	}
	if m.Eval == nil || m.Eval.Cases != 2 || m.Eval.Passed != 1 || m.Eval.Coverage != 0.75 {
		t.Fatalf("eval: %+v", m.Eval)
	}
	if got := m.Eval.Results[1].Missing; len(got) != 1 || got[0] != "never in the policy|also absent" {
		t.Errorf("missing: %v", got)
	}
	if m.Report == nil || m.Report.Eval == nil || m.Report.Eval.Passed != 1 {
		t.Errorf("report eval summary: %+v", m.Report)
	}

	units, err := p.Units(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 3 {
		t.Fatalf("units: %+v", units)
	}
	for _, u := range units {
		if u.Title == "CFO approval threshold" && strings.Join(u.Tags, ",") != "approval,expenses" && strings.Join(u.Tags, ",") != "expenses,approval" {
			t.Errorf("tags: %v", u.Tags)
		}
	}
}

func TestOpenRefusesOtherFiles(t *testing.T) {
	ctx := context.Background()
	stamp := func(appID, version int32) string {
		path := filepath.Join(t.TempDir(), "x.db")
		s, err := graph.Open(path, graph.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetFileFormat(ctx, appID, version); err != nil {
			t.Fatal(err)
		}
		s.SetMeta(ctx, "spec", `{"name":"X","role":"Y"}`)
		s.Close()
		return path
	}
	if _, err := Open(stamp(0x12345678, 1)); err == nil || !strings.Contains(err.Error(), "not a persona pack") {
		t.Errorf("foreign application id: %v", err)
	}
	if _, err := Open(stamp(PackApplicationID, PackFormat+1)); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("newer format: %v", err)
	}
}

func TestSpecIDAndVersion(t *testing.T) {
	for _, tc := range []struct {
		spec Spec
		ok   bool
	}{
		{Spec{Name: "Fina Finance", Role: "r"}, true},
		{Spec{Name: "Fina", Role: "r", ID: "Fina"}, false},
		{Spec{Name: "Fina", Role: "r", Version: "1.0"}, false},
		{Spec{Name: "Fina", Role: "r", Version: "v1.0.0"}, false},
		{Spec{Name: "Fina", Role: "r", Version: "1.2.0-rc.1"}, true},
		{Spec{Name: "", Role: "r"}, false},
	} {
		s := tc.spec
		if err := s.normalize(); (err == nil) != tc.ok {
			t.Errorf("%+v: err %v", tc.spec, err)
		}
	}
	s := Spec{Name: "  Fina Finance! ", Role: "r"}
	s.normalize()
	if s.ID != "fina-finance" || s.Version != "0.1.0" {
		t.Errorf("defaults: %+v", s)
	}
	if CompareVersions("1.10.0", "1.9.0") != 1 || CompareVersions("1.0.0-rc.1", "1.0.0") != -1 {
		t.Error("CompareVersions does not follow semver")
	}
}

func TestMissingIgnoresCaseSpacesAndPunctuation(t *testing.T) {
	text := "Disetujui CFO bila di atas Rp25.000.000; tutup buku pada Hari Kerja ke 5."
	got := missing(text, []string{"rp 25.000.000", "hari kerja ke-5", "Direktur Utama|CFO", "Dirut"})
	if len(got) != 1 || got[0] != "Dirut" {
		t.Errorf("missing: %v", got)
	}
}
