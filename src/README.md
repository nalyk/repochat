# RepoChat v0.1.0

**Un singur command pentru a discuta în ChatGPT Web cu repository-ul Git local (Linux).**

RepoChat este un binar Go mic, fără dependențe externe la runtime pentru MCP, care expune doar instrumente de *citire* pentru repository-ul în care a fost lansat. Pentru legătura sigură cu ChatGPT Web folosește **OpenAI Secure MCP Tunnel** prin clientul oficial `tunnel-client`, instalat o singură dată.

> Status: MCP local și operațiile Git au teste automate; **nu a fost efectuat un test real end-to-end în ChatGPT**, deoarece acesta necesită un tunnel ID și un runtime key create în contul tău. Este un MVP, nu un produs auditat pentru medii ostile.

## Experiența de fiecare zi

```bash
cd ~/projects/proiectul-meu
repochat
```

Apoi, în interfața **web** ChatGPT, selectezi pluginul privat `@RepoChat` și poți cere:

- „Citește structura repository-ului. Explică-mi arhitectura.”
- „Găsește implementările autentificării și semnalează potențiale probleme.”
- „Ce fișiere sunt modificate și care sunt ultimele 10 commituri?”

Nu este necesar Codex, ChatGPT Desktop, un Git remote sau upload de arhive. RepoChat nu deschide niciun port pe PC.

## Instalare (o singură dată)

### 1. Instalează binarul

Pentru Linux x86_64:

```bash
mkdir -p "$HOME/.local/bin"
install -m 0755 repochat-linux-amd64 "$HOME/.local/bin/repochat"
# Verifică dacă ~/.local/bin există în PATH:
repochat --version
```

Altfel, invocă `$HOME/.local/bin/repochat` explicit.

### 2. Instalează clientul oficial OpenAI `tunnel-client`

Descarcă versiunea oficială pentru Linux din pagina de release:

https://github.com/openai/tunnel-client/releases/latest

Extrage executabilul `tunnel-client` și pune-l în `~/.local/bin/`. Rulează:

```bash
tunnel-client help quickstart
```

**Atenție:** RepoChat este un singur command *după configurare*, dar folosește oficialul `tunnel-client` ca proces copil. Nu pretinde că poate conecta un browser web la `localhost` fără un mecanism de transport autorizat.

### 3. Creează tunnel-ul

În contul OpenAI Platform, deschide:

https://platform.openai.com/settings/organization/tunnels

1. Creează un **Secure MCP Tunnel**; copiază `tunnel_id`.
2. Asociază tunnel-ul cu **ChatGPT workspace-ul** în care vrei să folosești pluginul. Organizația Platform și workspace-ul ChatGPT sunt contexte distincte.
3. Creează o **runtime API key** cu permisiunea **Tunnels Read + Use**. Nu folosi o cheie admin pentru runtime.

Resurse: https://developers.openai.com/api/docs/guides/secure-mcp-tunnels și https://github.com/openai/tunnel-client/blob/master/docs/onboarding.md

### 4. Salvează credentialele local

```bash
repochat setup
```

Comanda solicită tunnel ID-ul și runtime API key; cheia este citită fără echo. Configurația se stochează în `$XDG_CONFIG_HOME/repochat/config.json` sau `~/.config/repochat/config.json`, cu **permisiuni 0600**, în afara oricărui repository. RepoChat refuză încărcarea unui fișier de configurare accesibil altor utilizatori.

Alternativ, pentru CI sau un password manager, poți seta variabilele de mediu `CONTROL_PLANE_TUNNEL_ID` și `CONTROL_PLANE_API_KEY` înainte de lansarea comenzii `repochat` (fără configurare salvată).

Verifică fără a afișa cheia:

```bash
cd ~/projects/proiectul-meu
repochat doctor
```

### 5. Creează pluginul privat în ChatGPT Web

În https://chatgpt.com : **Settings → Plugins → + → Add custom MCP server**.

- Name: `RepoChat`
- Connection: **Tunnel**
- Alege `tunnel_id` creat la pasul 3.
- Authentication: **No authentication** pentru serverul MCP local din acest MVP (runtime key autorizează separat *tunnel-client*, nu este un OAuth token de aplicație). Revizuiește avertizarea privind instrumentele MCP.
- **Create as a plugin**, apoi instalează pluginul.

Pornește `repochat` **înainte de crearea sau actualizarea conexiunii**, deoarece ChatGPT trebuie să poată descoperi instrumentele.

Pentru a-l folosi într-un chat, selectează `@RepoChat`. Dacă toolurile nu apar, verifică `tunnel-client` readiness (`/readyz` / admin UI) și folosește **Refresh tools** din setările pluginului.

## Instrumentele MCP

| Instrument | Ce poate face |
|---|---|
| `repo_info` | Rădăcina repository-ului, branch-ul, commitul HEAD |
| `tree` | Lista de directoare/fișiere, cu limite de profunzime |
| `read_file` | Citește surse text UTF-8 cu numere de linie |
| `search_code` | Caută literal în fișiere text, cu limite de volum |
| `git_status` | Afișează working tree-ul, inclusiv modificări necomise |
| `git_log` | Afișează ultimele commituri, numai metadate |

Nu există instrumente de **write**, `shell`, rulare arbitrară de comenzi, `git push`, `git diff` sau acces la alte directoare. Git este executat exclusiv cu subcomenzi predefinite, fără argumente arbitrare primite de la model.

## Securitate și limitări

- Root-ul este **strict directorul rădăcină al repository-ului** din care pornești; pornirea din subdirector este respinsă.
- `..`, căile absolute, symlink-urile care ies din root, `.git/`, directoarele generate obișnuite și multe nume asociate secretelor (`.env`, `.pem`, `credentials` etc.) sunt blocate.
- **Filtrarea după nume nu garantează că nu vor fi expuse secrete introduse în alte fișiere**. Conținutul cerut de ChatGPT va fi transmis prin OpenAI și poate fi procesat conform politicilor serviciului. Folosește-l doar cu cod pe care ai dreptul să-l divulgi.
- Nu rula concomitent două instanțe `repochat` cu același `tunnel_id`. Oprește cu `Ctrl+C` înainte de a schimba repository-ul.
- Executarea `git status`/`git log` în repo-uri malițioase nu are aceeași izolare precum un container/sandbox. Acest MVP trebuie folosit numai cu proiecte de încredere.
- Toolurile sunt read-only, dar accesul la repository-ul din lucru este „live”. Nu există index sau cache semantic; căutarea este literală.
- Nu există niciun daemon autonom: când procesul este oprit sau PC-ul nu are conexiune, ChatGPT nu poate accesa repository-ul.
- Tool discovery și autentificarea pluginurilor pot depinde de permisiunile workspace-ului. Acestea nu pot fi configurate de un simplu program local.

## Verificare locală, fără ChatGPT

Din surse:

```bash
go test -v ./...
go build -o repochat .
```

Pentru a interoga protocolul MCP prin stdio:

```bash
cd /path/to/your/git/repo
printf '%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
 '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repo_info","arguments":{}}}' \
 | repochat mcp
```

## Implementare

Go 1.21+, standard library. **Zero dependențe terțe în build-ul MCP**. Executabilul este compilat static (`CGO_ENABLED=0`) pentru Linux AMD64. `tunnel-client` este un executabil separat, oficial OpenAI; RepoChat îl lansează automat cu o conexiune MCP stdio și trimite cheia **prin mediu**, nu prin argv.

Licență propusă pentru surse: MIT (cod original generat pentru acest MVP). Verifică termenii și licențele `tunnel-client` separat.
