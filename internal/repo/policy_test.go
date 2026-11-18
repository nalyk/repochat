package repo

import (
	"strings"
	"testing"
)

func TestBuiltinSecretRules(t *testing.T) {
	served := []string{
		"token.go", "auth_token.py", "password.service.ts", "src/secrets/manager.rs", "lib/credentials_provider.java",
		"api_key_validator.rb", "README.md", "docs/passwords.md", ".env.example", ".env.sample", "config/.env.template",
		"deploy/app.env.dist", "id_rsa.pub", "keys/id_ed25519.pub", "secrets.schema.json", "fixtures/token.example.json",
		"package.json", "tsconfig.json", "public.key.md", "tokenizer.json", "secret_santa.py", "Makefile", ".gitignore",
		"terraform/main.tf", "config.yaml", "monkey.txt", "private_key_test.go",
	}
	withheld := []string{
		".env", ".env.local", ".env.production", "config/.env", "prod.env", ".envrc", ".npmrc", ".pypirc", ".netrc",
		".git-credentials", "id_rsa", "deploy/id_ed25519", "id_ecdsa-old", "server.pem", "tls/server.key", "cert.p12",
		"store.jks", "app.keystore", "vault.kdbx", "terraform.tfstate", "env/terraform.tfstate.backup",
		"prod.tfvars", "prod.tfvars.json", "service-account.json", "gcp-service_account.json", "client_secret_123.json",
		"firebase-adminsdk-abc.json", ".htpasswd", "secrets.yaml", "config/secrets.yml", "credentials.json",
		"credentials", ".aws/credentials", "home/.ssh/config", ".ssh/known_hosts", ".gnupg/pubring.kbx",
		"passwords.txt", "test/fixtures/token.json", "api-keys.txt", "api_key.json", "private-key.txt", "token", "tokens.json", ".secrets",
		".docker/config.json", ".kube/config", "wp-config.php", "config/master.key", "SECRETS.YAML", "Passwords.TXT",
	}
	for _, p := range served {
		if builtinSecret(p) {
			t.Errorf("%s should be served but the built-in rules withhold it", p)
		}
	}
	for _, p := range withheld {
		if !builtinSecret(p) {
			t.Errorf("%s should be withheld by the built-in rules", p)
		}
	}
}

func globs(t *testing.T, patterns ...string) []*Glob {
	t.Helper()
	g, err := CompileGlobs(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGlobSemantics(t *testing.T) {
	cases := []struct {
		pattern string
		yes, no []string
	}{
		{"*.go", []string{"main.go", "a/b/c.go"}, []string{"main.goo", "go", "a/b.go.txt"}},
		{"build", []string{"build", "build/x", "a/build/y/z"}, []string{"builder", "a/rebuild"}},
		{"docs/", []string{"docs/a.md", "x/docs/b"}, []string{"docs"}},
		{"/Makefile", []string{"Makefile"}, []string{"a/Makefile"}},
		{"src/*.ts", []string{"src/a.ts"}, []string{"src/a/b.ts", "x/src/a.ts"}},
		{"src/**/*.ts", []string{"src/a.ts", "src/a/b/c.ts"}, []string{"lib/a.ts"}},
		{"**/test_*.py", []string{"test_a.py", "x/y/test_b.py"}, []string{"x/atest_b.py"}},
		{"a/**", []string{"a/b", "a/b/c"}, []string{"b/a"}},
		{"file?.txt", []string{"file1.txt", "d/fileA.txt"}, []string{"file.txt", "file12.txt"}},
		{"[ab]*.md", []string{"a.md", "bz.md"}, []string{"c.md"}},
		{"[!a]x", []string{"bx"}, []string{"ax"}},
		{"data[[:digit:]].txt", []string{"data1.txt", "d/data9.txt"}, []string{"dataX.txt", "data12.txt"}},
		{"[[:upper:][:digit:]]*.md", []string{"A.md", "7x.md"}, []string{"a.md"}},
		{"[^[:alpha:]]x", []string{"1x"}, []string{"ax"}},
		{"a[!b]c", []string{"axc"}, []string{"abc", "a/c"}},
		{"[]]x", []string{"]x"}, []string{"ax"}},
		{"[a-c]1", []string{"b1"}, []string{"d1"}},
		{`[\]a]z`, []string{"]z", "az"}, []string{"bz"}},
		{`weird\*name`, []string{"weird*name"}, []string{"weirdXname"}},
		{"a.b", []string{"a.b"}, []string{"aXb"}},
	}
	for _, tc := range cases {
		g, err := CompileGlob(tc.pattern)
		if err != nil {
			t.Fatalf("%s: %v", tc.pattern, err)
		}
		for _, p := range tc.yes {
			if !g.Match(p) {
				t.Errorf("%q should match %q", tc.pattern, p)
			}
		}
		for _, p := range tc.no {
			if g.Match(p) {
				t.Errorf("%q should not match %q", tc.pattern, p)
			}
		}
	}
	for _, bad := range []string{"", "/", "   ", strings.Repeat("a", 600), "a[bc", "x[[:digit:]", "[[:bogus:]]"} {
		if _, err := CompileGlob(bad); err == nil {
			t.Errorf("pattern %q should be rejected", bad)
		}
	}
}

func TestPolicyPrecedence(t *testing.T) {
	ignore := "# comment\n\ninternal/private/\n*.sql\n!schema.sql\n\\#literal\n"
	p, err := NewPolicy(ignore, globs(t, "*.csv", "vendor/"), globs(t, ".env.ci", "*.sql", "data.csv", "internal/private/ok.go"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"main.go":                 "",
		"internal/private/x.go":   ReasonIgnoreFile,
		"internal/private/ok.go":  ReasonIgnoreFile, // --allow never overrides .repochatignore
		"dump.sql":                ReasonIgnoreFile, // --allow never overrides .repochatignore
		"schema.sql":              "",               // negated in the ignore file
		"db/schema.sql":           "",
		"#literal":                ReasonIgnoreFile,
		"data.csv":                ReasonDenyFlag, // --deny beats --allow
		"vendor/lib/a.go":         ReasonDenyFlag,
		".env.ci":                 "", // --allow overrides a built-in rule
		".env":                    ReasonSecretName,
		"bad\x01name":             ReasonUnsupported,
		"bad\xffname":             ReasonUnsupported,
		"line\nbreak":             ReasonUnsupported,
		"ünïcödé/файл.go":         "",
		"dir with space/file.txt": "",
	}
	for path, want := range cases {
		if got := p.Check(path); got != want {
			t.Errorf("Check(%q) = %q, want %q", path, got, want)
		}
	}
	// A line that cannot be understood must stop everything: skipping it
	// would serve what it was written to withhold.
	for _, bad := range []string{strings.Repeat("x", 600), "data[0-9.txt", "x[[:nope:]]"} {
		if _, err := NewPolicy("ok.txt\n"+bad+"\n", nil, nil); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("ignore line %q must be an error naming the line, got %v", bad, err)
		}
	}
}

func TestPolicyHashChangesWithRules(t *testing.T) {
	a, _ := NewPolicy("", nil, nil)
	b, _ := NewPolicy("x\n", nil, nil)
	c, _ := NewPolicy("", globs(t, "x"), nil)
	if a.hash == b.hash || a.hash == c.hash || b.hash == c.hash {
		t.Error("different policies must have different hashes")
	}
}

func TestCheckPath(t *testing.T) {
	good := map[string]string{
		"a/b.go": "a/b.go", "./a/b.go": "a/b.go", "a/b/": "a/b", " a.txt ": "a.txt", `dir\file.go`: "dir/file.go",
	}
	for in, want := range good {
		got, err := CheckPath(in)
		if err != nil || got != want {
			t.Errorf("CheckPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "/etc/passwd", "../x", "a/../../x", "a/./b", "a//b", ".git/config", "x/.git/y", "~/x", "a\x00b", "a\nb"} {
		if got, err := CheckPath(bad); err == nil {
			t.Errorf("CheckPath(%q) should fail, got %q", bad, got)
		}
	}
}

func FuzzCheckPath(f *testing.F) {
	for _, seed := range []string{"a/b", "../x", "/abs", ".git/config", "a/./b", "x\\y", "a//b", "..", "a/..", "é/ü"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, err := CheckPath(in)
		if err != nil {
			return
		}
		if out == "" || strings.HasPrefix(out, "/") || !SupportedName(out) {
			t.Fatalf("CheckPath(%q) accepted %q", in, out)
		}
		for _, seg := range strings.Split(out, "/") {
			if seg == ".." || seg == "." || seg == "" || seg == ".git" {
				t.Fatalf("CheckPath(%q) accepted %q with segment %q", in, out, seg)
			}
		}
	})
}

func FuzzGlob(f *testing.F) {
	for _, seed := range []string{"*.go", "a/**/b", "[a-z]*", "[!x]", `\*`, "**", "a/", "[", "]]", "**/**/**", "a[b"} {
		f.Add(seed, "some/path/file.go")
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		g, err := CompileGlob(pattern)
		if err != nil {
			return
		}
		g.Match(path) // must not panic
	})
}

func FuzzRevision(f *testing.F) {
	for _, seed := range []string{"HEAD", "main~2", "v1.0^{commit}", "-x", "--all", "a..b", "@{-1}", "x y", "HEAD:path", "a;b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, rev string) {
		if !revRe.MatchString(rev) {
			return
		}
		if strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, " \t\n:;|&$`\\\"'<>*?[]()") {
			t.Fatalf("revision pattern accepted %q", rev)
		}
	})
}
