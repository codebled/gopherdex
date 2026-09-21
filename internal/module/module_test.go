package module

import (
	"errors"
	"slices"
	"testing"
)

func TestCheckPath(t *testing.T) {
	valid := []string{"example.com/hello", "github.com/go-chi/chi/v5", "gopkg.in/yaml.v3", "github.com/Azure/azure-sdk-for-go"}
	for _, p := range valid {
		if err := CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v, want nil", p, err)
		}
	}
	invalidPaths := []string{"", "hello", "/example.com/x", "example.com/x/", "example.com//x", "example.com/../x", "example.com/a b", "example.com/@v", "-bad.com/x"}
	for _, p := range invalidPaths {
		if err := CheckPath(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("CheckPath(%q) = %v, want ErrInvalid", p, err)
		}
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	got, err := EscapePath("github.com/Azure/azure-sdk-for-go")
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/!azure/azure-sdk-for-go"; got != want {
		t.Fatalf("EscapePath = %q, want %q", got, want)
	}
	back, err := UnescapePath(got)
	if err != nil || back != "github.com/Azure/azure-sdk-for-go" {
		t.Fatalf("UnescapePath(%q) = %q, %v", got, back, err)
	}
	for _, bad := range []string{"github.com/Azure/x", "github.com/!/x", "github.com/x!"} {
		if _, err := UnescapePath(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("UnescapePath(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"v0.0.1", "v1.2.3", "v1.2.3-beta.1", "v2.0.0+incompatible", "v0.0.0-20240101120000-abcdef123456"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("CheckVersion(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"", "1.2.3", "v1.2", "v01.2.3", "v1.2.3-", "v1.2.3-01", "v1.2.3+build", "latest"} {
		if err := CheckVersion(v); !errors.Is(err, ErrInvalid) {
			t.Errorf("CheckVersion(%q) = %v, want ErrInvalid", v, err)
		}
	}
}

func TestSort(t *testing.T) {
	got := []string{"v1.10.0", "v1.2.0", "v1.2.0-rc.1", "v1.2.0-beta.2", "v1.2.0-beta.10", "v1.2.0-alpha", "v0.9.0", "v1.2.0-beta"}
	Sort(got)
	want := []string{"v0.9.0", "v1.2.0-alpha", "v1.2.0-beta", "v1.2.0-beta.2", "v1.2.0-beta.10", "v1.2.0-rc.1", "v1.2.0", "v1.10.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("Sort = %v\nwant   %v", got, want)
	}
}

func TestLatest(t *testing.T) {
	tests := []struct {
		versions []string
		want     string
	}{
		{[]string{"v1.0.0", "v1.1.0", "v1.2.0-beta.1"}, "v1.1.0"},
		{[]string{"v1.0.0-rc.1", "v1.0.0-rc.2"}, "v1.0.0-rc.2"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := Latest(tt.versions); got != tt.want {
			t.Errorf("Latest(%v) = %q, want %q", tt.versions, got, tt.want)
		}
	}
}
