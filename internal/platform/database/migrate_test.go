package database

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fictionthai/fictionthai/backend/migrations"
)

func TestParseFilename(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		wantVersion int64
		wantLabel   string
		wantErr     bool
	}{
		{"timestamped", "20260809000001_init_extensions.sql", 20260809000001, "init_extensions", false},
		{"short version", "2_add_users.sql", 2, "add_users", false},
		{"no separator", "20260809000001.sql", 0, "", true},
		{"non-numeric version", "init_extensions.sql", 0, "", true},
		{"zero version", "0_nothing.sql", 0, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version, label, err := parseFilename(tc.filename)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.filename)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFilename(%q) error = %v", tc.filename, err)
			}
			if version != tc.wantVersion {
				t.Errorf("version = %d, want %d", version, tc.wantVersion)
			}
			if label != tc.wantLabel {
				t.Errorf("label = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

func TestParseSections(t *testing.T) {
	content := `-- a leading comment outside any section is ignored
-- +migrate Up
CREATE TABLE example (id INT);

-- +migrate Down
DROP TABLE example;
`

	up, down, noTx, err := parseSections(content)
	if err != nil {
		t.Fatalf("parseSections() error = %v", err)
	}
	if !strings.Contains(up, "CREATE TABLE example") {
		t.Errorf("up section = %q, want the CREATE statement", up)
	}
	if strings.Contains(up, "DROP TABLE") {
		t.Error("the up section must not contain down statements")
	}
	if !strings.Contains(down, "DROP TABLE example") {
		t.Errorf("down section = %q, want the DROP statement", down)
	}
	if noTx {
		t.Error("NoTransaction should default to false")
	}
}

func TestParseSections_NoTransactionDirective(t *testing.T) {
	content := `-- +migrate NoTransaction
-- +migrate Up
CREATE INDEX CONCURRENTLY idx_example ON example (id);
`
	_, _, noTx, err := parseSections(content)
	if err != nil {
		t.Fatalf("parseSections() error = %v", err)
	}
	if !noTx {
		t.Error("expected the NoTransaction directive to be recognised")
	}
}

func TestParseSections_RequiresUp(t *testing.T) {
	tests := map[string]string{
		"no directives": "CREATE TABLE example (id INT);",
		"only down":     "-- +migrate Down\nDROP TABLE example;",
		"empty up":      "-- +migrate Up\n\n-- +migrate Down\nDROP TABLE example;",
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseSections(content); err == nil {
				t.Error("expected a migration without usable Up statements to be rejected")
			}
		})
	}
}

func TestParseSections_TolerantOfCRLF(t *testing.T) {
	content := "-- +migrate Up\r\nSELECT 1;\r\n-- +migrate Down\r\nSELECT 2;\r\n"

	up, down, _, err := parseSections(content)
	if err != nil {
		t.Fatalf("parseSections() error = %v", err)
	}
	if !strings.Contains(up, "SELECT 1") || !strings.Contains(down, "SELECT 2") {
		t.Errorf("CRLF files should parse: up=%q down=%q", up, down)
	}
}

func TestLoad_SortsByVersionNotFilename(t *testing.T) {
	body := "-- +migrate Up\nSELECT 1;\n-- +migrate Down\nSELECT 1;\n"
	fsys := fstest.MapFS{
		"10_ten.sql": {Data: []byte(body)},
		"2_two.sql":  {Data: []byte(body)},
		"1_one.sql":  {Data: []byte(body)},
	}

	set, err := load(fsys, ".")
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	// Lexical order would give 1, 10, 2 - numeric order is what determines
	// whether the schema ends up correct.
	want := []int64{1, 2, 10}
	if len(set) != len(want) {
		t.Fatalf("loaded %d migrations, want %d", len(set), len(want))
	}
	for i, version := range want {
		if set[i].Version != version {
			t.Errorf("position %d = version %d, want %d", i, set[i].Version, version)
		}
	}
}

func TestLoad_RejectsDuplicateVersions(t *testing.T) {
	body := "-- +migrate Up\nSELECT 1;\n"
	fsys := fstest.MapFS{
		"1_first.sql":  {Data: []byte(body)},
		"1_second.sql": {Data: []byte(body)},
	}

	if _, err := load(fsys, "."); err == nil {
		t.Fatal("two migrations sharing a version must be rejected: the apply order would be ambiguous")
	}
}

func TestLoad_IgnoresNonSQLFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"1_real.sql": {Data: []byte("-- +migrate Up\nSELECT 1;\n")},
		"README.md":  {Data: []byte("notes")},
	}

	set, err := load(fsys, ".")
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("loaded %d migrations, want 1", len(set))
	}
}

// The migrations that actually ship must parse. This catches a malformed file
// at test time rather than during a deployment.
func TestEmbeddedMigrationsAreValid(t *testing.T) {
	set, err := load(migrations.FS, migrations.Dir)
	if err != nil {
		t.Fatalf("the embedded migrations failed to load: %v", err)
	}
	if len(set) == 0 {
		t.Fatal("no migrations were embedded; check the //go:embed directive")
	}

	for _, m := range set {
		if strings.TrimSpace(m.Up) == "" {
			t.Errorf("migration %d (%s) has an empty Up section", m.Version, m.Name)
		}
		if strings.TrimSpace(m.Down) == "" {
			t.Errorf("migration %d (%s) has no Down section; rollback would be impossible", m.Version, m.Name)
		}
	}
}
