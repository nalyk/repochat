# RepoChat

Discuți în ChatGPT Web despre repository-ul Git de pe calculatorul tău, fără să-l urci nicăieri și fără să deschizi vreun port. RepoChat este doar pentru citire: prin el nu se poate modifica nimic.

```bash
cd ~/projects/proiectul-meu
repochat
```

Apoi, în ChatGPT: `@RepoChat ce face AuthenticationService și unde e folosit?`

RepoChat este un singur binar Go, fără dependențe externe. Vorbește protocolul MCP și ajunge la ChatGPT prin [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels), cu clientul oficial OpenAI `tunnel-client`, pe care îl instalează singur la prima pornire.

> **Ce trebuie să știi înainte.** Tot ce citește ChatGPT din repository ajunge la OpenAI. „Local” înseamnă aici că nu urci codul nicăieri dinainte și că legătura pleacă de pe calculatorul tău, nu că datele rămân pe el. Folosește RepoChat doar cu cod pe care ai voie să-l arăți.

## Pornire rapidă

### 1. Instalare

Din surse (ai nevoie de Go 1.24+ și git 2.25+):

```bash
make install          # compilează și copiază binarul în ~/.local/bin
```

sau dintr-o arhivă de release: dezarhivezi și rulezi `./repochat install`.

### 2. Prima pornire

Intri într-un repository și rulezi `repochat`. Programul te duce prin tot ce lipsește, în ordine:

1. **Verifică git și repository-ul.** Nu descarcă și nu cere nimic până nu știe că are ce servi.
2. **Instalează `tunnel-client`**, dacă nu îl găsește. Îți arată de unde descarcă și ce verifică, apoi întreabă. Versiunea implicită este fixată, iar arhiva este comparată cu sume SHA-256 compilate în RepoChat.
3. **Cere două valori de la OpenAI Platform**, o singură dată, într-un singur câmp ascuns (le lipești în orice ordine):
   - ID-ul unui tunel creat la <https://platform.openai.com/settings/organization/tunnels> și atașat workspace-ului tău ChatGPT;
   - o cheie API *Restricted*, cu permisiunile Tunnels: Read + Use, de la <https://platform.openai.com/settings/organization/api-keys>.

   RepoChat le verifică pe loc la OpenAI și le salvează într-un fișier pe care îl poți citi doar tu.
4. **Pornește tunelul și îți arată pasul din ChatGPT**, tot o singură dată: <https://chatgpt.com/plugins> → `+` → *Add custom MCP server* → Connection: **Tunnel** → alegi tunelul → *No authentication* → *Create as a plugin* (personal) → *Install*.
5. **Confirmă în terminal** când a servit primul apel venit din ChatGPT. Din acel moment configurarea e gata.

Pasul din ChatGPT nu poate fi făcut de un program local: tunelul și pluginul se creează în contul tău. Restul e automat.

### 3. Zi de zi

```bash
cd alt-proiect && repochat      # Ctrl+C oprește partajarea
```

Poți porni din orice subdirector. Dacă tunelul e deja folosit de alt RepoChat (alt repository, alt terminal), programul te întreabă dacă îl oprește pe acela și îl preia.

Fără terminal (scripturi, CI) nu se pune nicio întrebare: setezi `CONTROL_PLANE_TUNNEL_ID` și `CONTROL_PLANE_API_KEY` și adaugi `--yes` unde vrei confirmare implicită.

## Ce vede ChatGPT

| Vede | Nu vede |
|---|---|
| fișierele pe care git le cunoaște: urmărite și neurmărite | fișierele ignorate prin `.gitignore` |
| istoricul accesibil din `HEAD`, ramuri, ramuri remote și taguri | stash, reflog, commituri orfane |
| numărul fișierelor reținute, pe categorii | conținutul și numele fișierelor reținute |
| | directorul `.git`, fișiere din afara repository-ului, ținte de symlink |

**Fișiere reținute.** Fișierele care arată a secrete sunt reținute implicit: `.env`, `*.pem`, `*.key`, `id_rsa`, `terraform.tfstate`, `*.tfvars`, `credentials.json`, `secrets.yaml`, `.npmrc`, `service-account.json` și altele asemenea. Regula pe cuvinte-cheie (`token`, `secret`, `password`…) se aplică doar fișierelor de date și de configurare, deci `token.go` sau `password.service.ts` rămân vizibile. Modelul află că există fișiere reținute și câte sunt, ca să nu confunde „ascuns” cu „inexistent”.

**Mascare.** Valorile care arată a secrete în fișierele servite (chei AWS, tokenuri GitHub/GitLab/Slack, chei OpenAI/Anthropic/Google/Stripe, JWT, parole în URL-uri, chei private PEM) apar ca `[REDACTED:tip]`. Căutarea lucrează pe textul deja mascat, așa că un secret nu poate fi ghicit caracter cu caracter prin interogări. Mascarea este „best effort”: un secret într-un format necunoscut nu este detectat.

Vezi exact ce s-ar servi, fără să pornești nimic:

```bash
repochat files                 # fișierele servite
repochat files --withheld      # fișierele reținute și motivul
repochat call read_file '{"path":"README.md"}'    # exact textul pe care l-ar primi modelul
```

## Controlul expunerii

| Opțiune | Efect |
|---|---|
| `--tracked-only` | ascunde fișierele neurmărite |
| `--no-history` | doar arborele de lucru, fără commituri vechi |
| `--deny PATTERN` | reține în plus (sintaxă `.gitignore`, repetabil) |
| `--allow PATTERN` | servește un fișier pe care regulile implicite l-ar reține |
| `--show-withheld-names` | modelul vede numele fișierelor reținute, nu și conținutul |
| `--no-redact` | oprește mascarea valorilor |
| `--max-output-kb N` | bugetul unui răspuns (16–2048, implicit 200) |
| `--profile NUME` | alt tunel salvat |

Un fișier `.repochatignore` în repository (aceeași sintaxă ca `.gitignore`) reține fișiere pentru toți cei care servesc acel repository. El poate doar să strângă: `--allow` nu trece peste el.

Ordinea regulilor: nume nesuportat → `.repochatignore` → `--deny` → `--allow` → regulile implicite pentru secrete.

**Profile.** `repochat setup --profile client-x` salvează alt tunel. `repochat setup --bind` leagă un profil de repository-ul curent: profilul e ales automat acolo și refuză orice alt repository. E util când un tunel trebuie să servească un singur proiect.

## Comenzi

| Comandă | Ce face |
|---|---|
| `repochat` | partajează repository-ul curent |
| `repochat status` / `stop` | se partajează ceva acum? / oprește din alt terminal |
| `repochat doctor` | verifică separat fiecare parte și spune ce nu a putut testa |
| `repochat setup` | introduce sau schimbă tunelul și cheia; `--list`, `--remove`, `--bind` |
| `repochat files`, `tools`, `call` | ce se servește, ce instrumente primește modelul, rulează un instrument |
| `repochat log` | activitatea recentă (ce instrument, când, cât a durat) |
| `repochat mcp` | serverul MCP propriu-zis (stdio) |
| `repochat mcp-config` | configurare pentru alți clienți MCP |
| `repochat install`, `install-helper` | instalează binarul, respectiv `tunnel-client` |
| `repochat help chatgpt` | pașii din ChatGPT |

## Instrumentele pe care le primește modelul

Toate sunt marcate `readOnlyHint`, deci ChatGPT nu cere confirmare la fiecare apel.

| Instrument | Rol |
|---|---|
| `repo_overview` | ramură, HEAD, modificări necomise, structură, tipuri de fișiere, ce poate vedea sesiunea |
| `list_files`, `find_files` | navigare în arbore; găsire după nume sau tipar |
| `read_file`, `read_files` | citire cu numere de linie, total de linii și indicația de continuare; și la o revizie veche |
| `search_code` | căutare literală sau RE2, cu context, filtre și cursor; spune explicit când e incompletă |
| `file_outline` | definițiile dintr-un fișier (Go: analizor real; alte limbaje: euristic, etichetat ca atare) |
| `git_status`, `git_log`, `git_diff`, `git_show`, `git_blame`, `git_refs` | starea și istoricul |

Fiecare răspuns se termină cu o linie `@meta {json}`: dacă e complet, cursorul sau linia de continuare, câte valori au fost mascate.

## Securitate

**Ce garantează RepoChat (verificat prin teste automate):**

- Niciun instrument nu scrie și niciunul nu execută comenzi arbitrare. Modelul nu furnizează niciodată argumente brute pentru git.
- Un fișier este servit doar dacă git îl listează și politica îl admite. Symlink-urile nu sunt urmate nicăieri, nici dacă un director este înlocuit cu un symlink în timpul accesului.
- Un diff sau un commit care atinge doar fișiere reținute nu produce conținut. O listă goală de căi permise nu devine „toate căile”.
- Configurația din repository nu poate face o operație de citire să ruleze un program: filtre `clean`/`smudge`, `textconv`, drivere de diff, `fsmonitor`, `diff.external`, `GIT_EXTERNAL_DIFF`, descărcări automate printr-un remote. Fiecare caz este testat cu un control pozitiv: testul arată întâi că git simplu rulează programul.
- Cheia API nu apare în argumentele niciunui proces, nu ajunge în mediul serverului MCP și nu ajunge la git. `tunnel-client` primește un mediu construit dintr-o listă albă.
- Un singur RepoChat pe tunel, pe acest calculator. Preluarea se face cooperativ, printr-un socket privat; niciun proces nu este semnalizat după PID.
- Textul din repository (surse, mesaje de commit, nume de fișiere) este tratat ca date nesigure: caracterele de control sunt neutralizate înainte să ajungă la model sau în terminal.

**Ce nu garantează:**

- **Nu este un sandbox.** Configurația git din repository este tratată ca nesigură pentru execuție, ca măsură suplimentară. Dacă un proces local înlocuiește directorul repository-ului sau `.git` în timpul rulării, RepoChat se oprește când observă, dar nu promite mai mult. Configurația git a utilizatorului și a sistemului este considerată de încredere.
- **Mascarea nu e o garanție.** Verifică singur cu `repochat files` și `repochat call`.
- **Cine poate folosi pluginul poate citi repository-ul** cât timp rulează RepoChat. Creează pluginul ca *personal*. Tunelul nu știe ce conversație îl folosește: dacă treci la alt repository, conversațiile vechi îl văd pe cel nou. RepoChat te avertizează la schimbare.
- **Instrucțiuni ascunse în cod** (prompt injection) pot influența modelul. RepoChat le marchează ca date, dar decizia aparține modelului.
- `tunnel-client` deschide un port de diagnoză pe `127.0.0.1` (aleatoriu). RepoChat însuși nu ascultă pe rețea.

Jurnalul local (`repochat log`) reține doar metadate: instrument, rezultat, durată, dimensiune. Argumentele se înregistrează doar cu `--audit-args`. E un istoric pentru tine, nu o pistă de audit rezistentă la modificări.

## Depanare

Începe cu `repochat doctor`: verifică git, repository-ul, serverul MCP (într-un subproces real, în ambele versiuni de protocol), `tunnel-client`, forma credențialelor și accesul la tunel, și spune clar ce nu a putut testa.

| Simptom | Cauză probabilă |
|---|---|
| tunelul nu apare în ChatGPT | nu e atașat workspace-ului ChatGPT, sau adminul a restricționat serverele MCP personalizate |
| „OpenAI rejected the tunnel connection (HTTP 401/403/404)” | cheie greșită / fără Tunnels Read + Use / ID de tunel greșit; rulează `repochat setup` |
| instrumentele nu apar după o actualizare | în setările pluginului: *Refresh tools* |
| „does not answer” la preluare | a rămas un `tunnel-client` orfan; oprește-l și repornește |
| modelul nu găsește un fișier | e ignorat de `.gitignore` sau reținut; vezi `repochat files --withheld` |

`repochat --verbose` arată și jurnalul lui `tunnel-client`.

## Alți clienți MCP

`repochat mcp` este un server MCP obișnuit pe stdio și nu are nevoie de tunel sau de credențiale. `repochat mcp-config` tipărește configurarea pentru Claude Desktop, Claude Code, Cursor sau VS Code. Serverul vorbește atât reviziile cu handshake (2024-11-05 … 2025-11-25), cât și revizia fără stare 2026-07-28.

## Dezvoltare și starea verificării

```bash
make check            # vet, teste cu detectorul de curse, onboarding în pseudo-terminal
make fuzz             # fuzzing scurt pe validatorii de intrare
make dist             # arhive pentru linux/darwin × amd64/arm64 + sume de control
make test-contract    # suita de contract cu tunnel-client REAL și un control plane simulat
```

Ce acoperă testele automate: politica de expunere, mascarea, accesul la fișiere și cursele pe symlink-uri, căutarea, toate instrumentele git pe repository-uri sintetice (fără commituri, HEAD detașat, worktree legat, submodul, fișier lipsă, pointer LFS), configurații git ostile cu control pozitiv, protocolul MCP în ambele ere (negociere, erori, anulare, concurență, limite), lansatorul cap-coadă cu un `tunnel-client` simulat (argumente, mediu, stări, preluare, eșecuri) și prima pornire într-un pseudo-terminal real.

**Ce nu a fost verificat și trebuie făcut de tine:**

1. **Conexiunea reală cu ChatGPT.** Are nevoie de un tunel și de o cheie din contul tău. Dacă planul tău ChatGPT nu oferă Secure MCP Tunnel pentru pluginuri personale, RepoChat nu poate schimba asta.
2. **Suita de contract cu binarul real `tunnel-client`** (`make test-contract`). Este scrisă după documentația protocolului și rulează obligatoriu la release, dar nu a fost rulată în mediul în care a fost dezvoltat RepoChat, pentru că presupune descărcarea binarului. Până trece o dată, compatibilitatea cu `tunnel-client` real se sprijină pe citirea surselor lui (v0.0.21), nu pe o rulare.
3. **Instalatorul pe arhiva reală.** Regulile de extragere sunt testate pe arhive construite; structura arhivei oficiale a fost dedusă din scripturile de împachetare ale OpenAI.

Designul a fost revizuit adversarial de Codex CLI în mai multe runde; transcrierea este în [`docs/design/`](docs/design/).

## Licență

MIT. `tunnel-client` este un program separat, al OpenAI, sub licență Apache-2.0.
