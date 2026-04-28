package apps

import (
	"strconv"
	"strings"
	"testing"
)

func TestBaseSlug(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello world", "hello-world"},
		{"  Hello   World  ", "hello-world"},
		{"My App!", "my-app"},
		{"foo-bar", "foo-bar"},
		{"FOO_BAR", "foo-bar"},
		{"emoji 🚀 launch", "emoji-launch"},
		{"кириллица test", "test"},
		{"---multiple---dashes---", "multiple-dashes"},
		{"...", ""},
		{"", ""},
		{"a", "a"},
		{"123-go", "123-go"},
		{"trailing-", "trailing"},
		{"-leading", "leading"},
		{"slash/and:colon", "slash-and-colon"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := BaseSlug(tc.in)
			if got != tc.want {
				t.Errorf("BaseSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBaseSlugTruncates(t *testing.T) {
	long := strings.Repeat("a", 100)
	got := BaseSlug(long)
	if len(got) != SlugMaxLen {
		t.Errorf("expected length %d, got %d", SlugMaxLen, len(got))
	}
}

func TestBaseSlugTruncationStripsTrailingDash(t *testing.T) {
	// 49 'a's + '-' + 'b' is 51 chars; truncated to 50 ends in '-'
	// which we should strip.
	in := strings.Repeat("a", 49) + "-b"
	got := BaseSlug(in)
	if got != strings.Repeat("a", 49) {
		t.Errorf("expected trimmed dash, got %q", got)
	}
}

func TestSlugWithSuffix(t *testing.T) {
	cases := []struct {
		base string
		n    int
		want string
	}{
		{"foo", 1, "foo"},
		{"foo", 2, "foo-2"},
		{"foo", 10, "foo-10"},
		{"foo", 100, "foo-100"},
		// At the boundary: base 48 + "-2" = 50.
		{strings.Repeat("a", 48), 2, strings.Repeat("a", 48) + "-2"},
		// One past the boundary: base 49 + "-2" = 51 → truncate to fit.
		{strings.Repeat("a", 49), 2, strings.Repeat("a", 48) + "-2"},
		// Very high N still fits.
		{strings.Repeat("a", 49), 999, strings.Repeat("a", 46) + "-999"},
	}
	for _, tc := range cases {
		t.Run(tc.base+"_"+strconv.Itoa(tc.n), func(t *testing.T) {
			got := SlugWithSuffix(tc.base, tc.n)
			if got != tc.want {
				t.Errorf("SlugWithSuffix(%q, %d) = %q, want %q", tc.base, tc.n, got, tc.want)
			}
			if len(got) > SlugMaxLen {
				t.Errorf("result %q exceeds SlugMaxLen=%d", got, SlugMaxLen)
			}
		})
	}
}

func TestSlugWithSuffixTrimsTrailingDash(t *testing.T) {
	// 47 'a's + dash + dash + "2" — after truncation we'd get
	// 47 'a's + dash + "-2" with a doubled dash; the helper should
	// trim the trailing dash from base before suffixing so we get
	// "a..a-2" rather than "a..a--2".
	base := strings.Repeat("a", 47) + "--"
	got := SlugWithSuffix(base, 2)
	if strings.Contains(got, "--") {
		t.Errorf("expected no doubled dash in %q", got)
	}
}

func TestIsReserved(t *testing.T) {
	for _, s := range []string{"new", "settings", "builds", "delete", "api", "admin"} {
		if !IsReserved(s) {
			t.Errorf("expected %q to be reserved", s)
		}
	}
	for _, s := range []string{"foo", "newish", "myapp", "settingz"} {
		if IsReserved(s) {
			t.Errorf("expected %q to not be reserved", s)
		}
	}
}
