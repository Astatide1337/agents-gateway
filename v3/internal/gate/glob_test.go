package gate

import "testing"

func TestValidateRepoPathRejectsEscapesAndPlatformAbsoluteNames(t *testing.T) {
	valid := []string{"README.md", "src/main.go", "日本語/ファイル.go", "a-b.txt"}
	for _, value := range valid {
		if !ValidateRepoPath(value) {
			t.Errorf("valid path rejected: %q", value)
		}
	}
	invalid := []string{
		"",
		"/etc/passwd",
		"//server/share/file",
		"C:/repo/file.go",
		"c:repo/file.go",
		`src\\main.go`,
		"src/../secret.go",
		"../secret.go",
		"src/./main.go",
		"src//main.go",
		"src/main.go/",
		"src/\x00.go",
		string([]byte{0xff}),
	}
	for _, value := range invalid {
		if ValidateRepoPath(value) {
			t.Errorf("unsafe path accepted: %q", value)
		}
	}
}

func TestMatchGlobGitCompatibleDoubleStarAndSegmentWildcards(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"src/**", "src/main.go", true},
		{"src/**", "src/internal/main.go", true},
		{"src/**", "src", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "src/internal/main.go", true},
		{"**/*.go", "src/main.txt", false},
		{"foo/**/bar.go", "foo/bar.go", true},
		{"foo/**/bar.go", "foo/a/b/bar.go", true},
		{"foo/**/bar.go", "foo/a/b/bar.txt", false},
		{"foo/*/bar.go", "foo/a/bar.go", true},
		{"foo/*/bar.go", "foo/a/b/bar.go", false},
		{"docs/?.md", "docs/a.md", true},
		{"docs/?.md", "docs/ab.md", false},
		{"src/[ab].go", "src/a.go", true},
		{"src/[ab].go", "src/c.go", false},
		{"src/[!a].go", "src/b.go", true},
		{"src/[!a].go", "src/a.go", false},
		{"**", "a/deep/path.txt", true},
	}
	for _, test := range cases {
		if got := MatchGlob(test.pattern, test.path); got != test.want {
			t.Errorf("MatchGlob(%q, %q)=%v, want %v", test.pattern, test.path, got, test.want)
		}
	}
}

func TestMatchGlobRejectsUnsafeOrMalformedPatterns(t *testing.T) {
	unsafe := []struct {
		pattern string
		path    string
	}{
		{"/src/**", "src/main.go"},
		{"../**", "secret.go"},
		{"src/../**", "src/main.go"},
		{`src\\**`, "src/main.go"},
		{"src//**", "src/main.go"},
		{"src/[", "src/a"},
		{"!src/**", "src/a"},
		{"src/", "src/a"},
	}
	for _, test := range unsafe {
		if MatchGlob(test.pattern, test.path) {
			t.Errorf("unsafe or malformed glob matched: %q %q", test.pattern, test.path)
		}
	}
	if MatchGlob("**/*.go", "../main.go") {
		t.Fatal("glob matched traversal path")
	}
}

func FuzzValidateRepoPathNeverPanics(f *testing.F) {
	for _, seed := range []string{"src/main.go", "../x", "/x", "a\\b", "a\x00b", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_ = ValidateRepoPath(value)
	})
}

func FuzzMatchGlobNeverPanics(f *testing.F) {
	seeds := [][2]string{{"**/*.go", "main.go"}, {"src/**", "src/a/b"}, {"[", "x"}, {"../**", "x"}}
	for _, seed := range seeds {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, pattern, value string) {
		_ = MatchGlob(pattern, value)
	})
}
