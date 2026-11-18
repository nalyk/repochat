# Jurnal de modificări

## 1.0.0

Rescriere completă a MVP-ului 0.1.0 (păstrat în istoricul git, în primul commit).

- Prima pornire într-o singură comandă: instalarea verificată a `tunnel-client`, un singur câmp ascuns pentru tunel și cheie, verificare la OpenAI, confirmare la primul apel servit.
- 13 instrumente doar-citire, cu `git_diff`, `git_show`, `git_blame`, `git_refs`, `file_outline`, `find_files`, `read_files`; cursori și indicații de continuare.
- Lista de fișiere vine de la git: fișierele ignorate nu mai sunt expuse.
- Politică de expunere cu `.repochatignore`, `--deny`, `--allow`; fișierele reținute sunt numărate, nu ascunse fără urmă.
- Mascarea valorilor care arată a secrete; căutarea lucrează pe textul mascat.
- Server MCP pentru ambele ere de protocol (handshake și 2026-07-28), cu anulare, concurență și limite.
- Execuție git întărită, acces la fișiere fără urmărirea symlink-urilor, un singur proprietar pe tunel.
- `doctor`, `status`, `stop`, `files`, `tools`, `call`, `log`, `mcp-config`, `install`, `install-helper`.

Defecte din 0.1.0 corectate: schema JSON cu `"required": null`, `repo_info` pe repository fără commituri, negocierea de versiune care repeta orice, filtrul de secrete care ascundea cod sursă, căutarea care raporta „No matches” când se oprea la o limită.
