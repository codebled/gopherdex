package gomod

import "testing"

const sample = `// Deprecated: use example.com/hello/v2 instead.
module example.com/hello

go 1.23

require example.com/one v1.0.0

require (
	example.com/two v0.4.1
	example.com/three v1.9.0 // indirect
)

replace example.com/one => ../one

// Greeting panicked on empty names.
retract v1.0.0

retract (
	[v1.1.0, v1.1.3] // Published from the wrong branch.
)
`

func TestParse(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if f.Module != "example.com/hello" || f.Go != "1.23" {
		t.Fatalf("module/go = %q/%q", f.Module, f.Go)
	}
	if f.Deprecated != "use example.com/hello/v2 instead." {
		t.Errorf("Deprecated = %q", f.Deprecated)
	}
	if len(f.Require) != 3 || !f.Require[2].Indirect || f.Require[1].Path != "example.com/two" {
		t.Errorf("Require = %+v", f.Require)
	}
	if len(f.Retract) != 2 {
		t.Fatalf("Retract = %+v", f.Retract)
	}
	if r, ok := f.Retracted("v1.0.0"); !ok || r.Rationale != "Greeting panicked on empty names." {
		t.Errorf("Retracted(v1.0.0) = %+v, %v", r, ok)
	}
	if r, ok := f.Retracted("v1.1.2"); !ok || r.Rationale != "Published from the wrong branch." {
		t.Errorf("Retracted(v1.1.2) = %+v, %v", r, ok)
	}
	if _, ok := f.Retracted("v1.2.0"); ok {
		t.Error("v1.2.0 should not be retracted")
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{"go 1.23\n", "module a.com/x\nrequire (\n a.com/y v1.0.0\n", "module a.com/x\nretract [v1.0.0\n"} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", src)
		}
	}
}
