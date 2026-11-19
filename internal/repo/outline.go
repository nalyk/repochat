package repo

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"regexp"
	"strings"
)

const (
	outlineFileLimit = 2 << 20
	maxOutlineItems  = 400
)

// OutlineResult is the rendered structure of a file.
type OutlineResult struct {
	Text       string
	Items      int
	Truncated  bool // more definitions were found than are listed
	Language   string
	Exact      bool // true when produced by a real parser
	Redactions int
}

type outlineItem struct {
	line int
	text string
}

// Outline lists the definitions in a file with their line numbers, so a
// model can jump straight to the relevant range with read_file. Go files are
// parsed with the Go parser; every other language uses line patterns and is
// labelled as a heuristic.
func (r *Repo) Outline(ctx context.Context, p string) (*OutlineResult, error) {
	rel, entry, _, err := r.authorizeFile(ctx, p)
	if err != nil {
		return nil, err
	}
	if entry.Kind != KindRegular {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	op := r.newOpener()
	data, _, truncated, err := op.ReadAll(rel, outlineFileLimit)
	op.Close()
	if err != nil {
		return nil, describeErr(rel, err)
	}
	if truncated {
		return nil, fmt.Errorf("%s is larger than %s; use search_code to find definitions in it", rel, humanSize(outlineFileLimit))
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return nil, fmt.Errorf("%s is a binary file", rel)
	}
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	st := r.red.NewState()
	masked := make([]string, len(lines))
	masks := map[int]int{} // line number → values masked on that line
	for i, l := range lines {
		if i&1023 == 0 && ctx.Err() != nil {
			return nil, ctx.Err() // the caller gave up; do not keep its worker slot
		}
		var n int
		if masked[i], n = st.Masked(strings.TrimSuffix(l, "\r")); n > 0 {
			masks[i+1] = n
		}
	}

	ext := strings.ToLower(path.Ext(rel))
	base := strings.ToLower(path.Base(rel))
	res := &OutlineResult{}
	var items []outlineItem
	note := ""
	if ext == ".go" {
		res.Language, res.Exact = "Go", true
		items, note = goOutline(data)
		if items == nil {
			res.Exact = false
		}
		// These lines are rendered from the syntax tree of the raw source,
		// so they are masked here; each is a single line by construction.
		masks = map[int]int{}
		for i := range items {
			var n int
			items[i].text, n = r.red.Line(items[i].text)
			masks[items[i].line] = max(masks[items[i].line], n) // several names can share a line
		}
	}
	if items == nil {
		lang, ok := outlineLangs[ext]
		if !ok {
			lang, ok = outlineLangs[base]
		}
		if !ok {
			return nil, fmt.Errorf("no outline support for %s files; use read_file or search_code instead", strings.TrimPrefix(ext, "."))
		}
		res.Language = lang.name
		items = patternOutline(masked, lang)
	}

	var b strings.Builder
	kind := "heuristic line patterns — may miss or mislabel definitions"
	if res.Exact {
		kind = "parsed"
	}
	fmt.Fprintf(&b, "OUTLINE %s · %s (%s) · %d line(s) · %d definition(s)\n", rel, res.Language, kind, len(lines), len(items))
	if note != "" {
		fmt.Fprintf(&b, "[note: %s]\n", note)
	}
	width := len(fmt.Sprint(len(lines)))
	for i, it := range items {
		if i == maxOutlineItems {
			fmt.Fprintf(&b, "[… %d more definitions not listed]\n", len(items)-i)
			break
		}
		// Shortened only now that it is masked; see cutPoint.
		t := it.text
		if len(t) > 220 {
			t = t[:cutPoint(t, 220)] + " …"
		}
		fmt.Fprintf(&b, "%*d| %s\n", width, it.line, Sanitize(t))
		res.Redactions += shownMasks(t, masks[it.line])
	}
	if len(items) == 0 {
		b.WriteString("[no definitions recognised; use read_file]\n")
	} else {
		b.WriteString("[read a definition with read_file start_line=<line>]\n")
	}
	res.Items = len(items)
	res.Truncated = len(items) > maxOutlineItems
	res.Text = b.String()
	return res, nil
}

// goOutline renders declarations from the Go AST. Values are never shown
// (only names, types and signatures). It returns nil when the file cannot be
// parsed at all, so the caller can fall back to patterns.
func goOutline(src []byte) ([]outlineItem, string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if f == nil {
		return nil, ""
	}
	note := ""
	if err != nil {
		note = "the file has syntax errors; the outline may be incomplete"
	}
	render := func(n ast.Node) string {
		var buf bytes.Buffer
		if printer.Fprint(&buf, fset, n) != nil {
			return ""
		}
		return strings.Join(strings.Fields(buf.String()), " ")
	}
	line := func(p token.Pos) int { return fset.Position(p).Line }
	items := []outlineItem{{line(f.Package), "package " + f.Name.Name}}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			sig := "func "
			if d.Recv != nil && len(d.Recv.List) > 0 {
				sig += "(" + render(d.Recv.List[0].Type) + ") "
			}
			sig += d.Name.Name + strings.TrimPrefix(render(d.Type), "func")
			items = append(items, outlineItem{line(d.Pos()), sig})
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					desc := "type " + s.Name.Name
					if s.TypeParams != nil {
						desc += render(s.TypeParams)
					}
					switch t := s.Type.(type) {
					case *ast.StructType:
						desc += fmt.Sprintf(" struct (%d field(s))", t.Fields.NumFields())
					case *ast.InterfaceType:
						desc += fmt.Sprintf(" interface (%d method(s))", t.Methods.NumFields())
					default:
						desc += " " + render(s.Type)
					}
					items = append(items, outlineItem{line(s.Pos()), desc})
					if t, ok := s.Type.(*ast.InterfaceType); ok {
						for _, m := range t.Methods.List {
							for _, n := range m.Names {
								items = append(items, outlineItem{line(n.Pos()), "    " + n.Name + strings.TrimPrefix(render(m.Type), "func")})
							}
						}
					}
				case *ast.ValueSpec:
					kw := strings.ToLower(d.Tok.String())
					for _, n := range s.Names {
						if n.Name == "_" {
							continue
						}
						desc := kw + " " + n.Name
						if s.Type != nil {
							desc += " " + render(s.Type)
						}
						items = append(items, outlineItem{line(n.Pos()), desc})
					}
				}
			}
		}
	}
	return items, note
}

type outlineLang struct {
	name     string
	patterns []*regexp.Regexp
	skip     *regexp.Regexp // lines matching this are never definitions
}

func rx(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

var controlKeywords = regexp.MustCompile(`^\s*(?:\}\s*)?(?:if|else|for|foreach|while|switch|catch|return|do|try|throw|new|await|yield|using|lock|case|default|delete|typeof|sizeof|assert)\b`)

var (
	jsLang = outlineLang{name: "JavaScript/TypeScript", skip: controlKeywords, patterns: rx(
		`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:async\s+)?function\s*\*?\s*[\w$]+`,
		`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?class\s+[\w$]+`,
		`^\s*(?:export\s+)?(?:declare\s+)?(?:interface|enum|namespace|module)\s+[\w$.]+`,
		`^\s*(?:export\s+)?(?:declare\s+)?type\s+[\w$]+\s*(?:<[^=]*>)?\s*=`,
		`^\s*(?:export\s+)?(?:const|let|var)\s+[\w$]+\s*(?::[^=]+)?=\s*(?:async\s*)?(?:function\b|\([^)]*\)\s*(?::[^=]+)?=>|[\w$]+\s*=>)`,
		`^\s+(?:(?:public|private|protected|static|readonly|abstract|override|async|get|set)\s+)*\*?[\w$#]+\s*(?:<[^>]*>)?\([^)]*\)\s*(?::\s*[^{;]+)?\{\s*$`,
		`^\s*(?:describe|it|test)(?:\.\w+)?\(\s*['"`+"`"+`]`,
	)}
	javaLang = outlineLang{name: "Java/C#/Kotlin-style", skip: controlKeywords, patterns: rx(
		`^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|private|protected|internal|static|final|abstract|sealed|partial|open|data|inner|strictfp|non-sealed)\s+)*(?:class|interface|enum|record|struct|object|@interface)\s+\w+`,
		`^\s*(?:(?:public|private|protected|internal|override|open|abstract|suspend|inline|operator|infix|tailrec|external)\s+)*fun\s+(?:<[^>]*>\s*)?[\w.]+\s*\(`,
		`^\s*(?:(?:public|private|protected|internal|static|final|abstract|override|virtual|async|sealed|synchronized|native|extern|unsafe|new|partial|default)\s+)+(?:[\w<>\[\],.?]+\s+)+\w+\s*\([^;]*$`,
		`^\s*namespace\s+[\w.]+`,
	)}
	cLang = outlineLang{name: "C/C++", skip: controlKeywords, patterns: rx(
		`^\s*(?:template\s*<[^>]*>\s*)?(?:class|struct|union|enum(?:\s+class)?|namespace)\s+\w+[^;]*$`,
		`^\s*typedef\s+.*\b\w+\s*;`,
		`^#\s*define\s+\w+\(`,
		`^[A-Za-z_][\w\s\*&:<>,~\[\]]*[\s\*&][\w:~]+\s*\([^;]*\)\s*(?:const\s*)?(?:noexcept\s*)?(?:override\s*)?\{?\s*$`,
	)}
)

var outlineLangs = map[string]outlineLang{
	".py": {name: "Python", patterns: rx(`^\s*(?:async\s+def|def|class)\s+\w+`)},
	".js": jsLang, ".jsx": jsLang, ".mjs": jsLang, ".cjs": jsLang, ".ts": jsLang, ".tsx": jsLang, ".mts": jsLang, ".cts": jsLang,
	".vue": jsLang, ".svelte": jsLang,
	".java": javaLang, ".cs": javaLang, ".kt": javaLang, ".kts": javaLang, ".scala": javaLang, ".groovy": javaLang, ".dart": javaLang,
	".c": cLang, ".h": cLang, ".cc": cLang, ".cpp": cLang, ".cxx": cLang, ".hpp": cLang, ".hh": cLang, ".m": cLang, ".mm": cLang,
	".rs": {name: "Rust", patterns: rx(
		`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+\w+`,
		`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|union|mod|type)\s+\w+`,
		`^\s*(?:unsafe\s+)?impl\b`,
		`^\s*macro_rules!\s*\w+`,
	)},
	".rb": {name: "Ruby", patterns: rx(`^\s*(?:def|class|module)\s+[\w.:?!=]+`)},
	".php": {name: "PHP", patterns: rx(
		`^\s*(?:abstract\s+|final\s+|readonly\s+)*(?:class|interface|trait|enum)\s+\w+`,
		`^\s*(?:(?:public|private|protected|static|abstract|final)\s+)*function\s+&?\w+`,
		`^\s*namespace\s+[\w\\]+`,
	)},
	".swift": {name: "Swift", patterns: rx(
		`^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|private|internal|fileprivate|open|static|class|final|override|mutating|nonisolated|convenience|required)\s+)*(?:func|class|struct|enum|protocol|extension|actor|typealias|init\b|deinit\b)\s*[\w.]*`,
	)},
	".sh": shellLang, ".bash": shellLang, ".zsh": shellLang,
	".sql": {name: "SQL", patterns: rx(`(?i)^\s*(?:create|alter)\s+(?:or\s+replace\s+)?(?:unique\s+)?(?:materialized\s+)?(?:table|view|function|procedure|index|trigger|type|schema|sequence)\s+`)},
	".md":  markdownLang, ".markdown": markdownLang, ".mdx": markdownLang,
	".ex":        {name: "Elixir", patterns: rx(`^\s*(?:defmodule|defp?|defmacrop?|defprotocol|defimpl)\s+[\w.?!]+`)},
	".exs":       {name: "Elixir", patterns: rx(`^\s*(?:defmodule|defp?|defmacrop?|defprotocol|defimpl)\s+[\w.?!]+`)},
	".lua":       {name: "Lua", patterns: rx(`^\s*(?:local\s+)?function\s+[\w.:]+`, `^\s*(?:local\s+)?[\w.]+\s*=\s*function\b`)},
	".tf":        {name: "Terraform", patterns: rx(`^\s*(?:resource|data|module|variable|output|provider|locals|terraform)\b[^=]*\{`)},
	".proto":     {name: "Protocol Buffers", patterns: rx(`^\s*(?:message|service|enum|rpc)\s+\w+`)},
	"makefile":   {name: "Makefile", patterns: rx(`^[A-Za-z0-9_.%/-][A-Za-z0-9_.%/ -]*:(?:[^=]|$)`)},
	"dockerfile": {name: "Dockerfile", patterns: rx(`(?i)^\s*(?:FROM|ENTRYPOINT|CMD|EXPOSE)\b`)},
}

var (
	shellLang    = outlineLang{name: "Shell", patterns: rx(`^\s*(?:function\s+)?[\w:.-]+\s*\(\)\s*\{?`, `^\s*function\s+[\w:.-]+`)}
	markdownLang = outlineLang{name: "Markdown", patterns: rx(`^#{1,6}\s+\S`)}
)

func patternOutline(lines []string, lang outlineLang) []outlineItem {
	var items []outlineItem
	fenced := false
	for i, l := range lines {
		if lang.name == "Markdown" {
			if strings.HasPrefix(strings.TrimSpace(l), "```") {
				fenced = !fenced
				continue
			}
			if fenced {
				continue
			}
		}
		if len(l) > 1000 || (lang.skip != nil && lang.skip.MatchString(l)) {
			continue
		}
		for _, re := range lang.patterns {
			if re.MatchString(l) {
				items = append(items, outlineItem{i + 1, strings.TrimRight(l, " \t{")})
				break
			}
		}
	}
	return items
}
