package watchns

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ", nil},
		{"a", []string{"a"}},
		{"a, b ,a,,c", []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		if got := Parse(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCacheOptions(t *testing.T) {
	if got := CacheOptions(nil); got.DefaultNamespaces != nil {
		t.Errorf("empty list must leave DefaultNamespaces nil (cluster-wide), got %v", got.DefaultNamespaces)
	}
	got := CacheOptions([]string{"x", "y"})
	if len(got.DefaultNamespaces) != 2 {
		t.Fatalf("want 2 namespaces, got %v", got.DefaultNamespaces)
	}
	for _, ns := range []string{"x", "y"} {
		if _, ok := got.DefaultNamespaces[ns]; !ok {
			t.Errorf("missing namespace %q", ns)
		}
	}
}
