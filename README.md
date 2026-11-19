# repochat

Proiect: [`nalyk/repochat`](https://github.com/nalyk/repochat) · <https://github.com/nalyk/repochat>

Discuți în ChatGPT Web despre repository-ul Git de pe calculatorul tău, fără să-l urci nicăieri și fără să deschizi vreun port. repochat este doar pentru citire: prin el nu se poate modifica nimic.

```bash
cd ~/projects/proiectul-meu
repochat
```

Apoi, în ChatGPT: `@RepoChat ce face AuthenticationService și unde e folosit?`

repochat este un singur binar Go, fără dependențe externe. Vorbește protocolul MCP și ajunge la ChatGPT prin [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels), cu clientul oficial OpenAI `tunnel-client`, pe care îl instalează singur la prima pornire.

> **Ce trebuie să știi înainte.** Tot ce citește ChatGPT din repository ajunge la OpenAI. „Local” înseamnă aici că nu urci codul nicăieri dinainte și că legătura pleacă de pe calculatorul tău, nu că datele rămân pe el. Folosește repochat doar cu cod pe care ai voie să-l arăți.

## Pornire rapidă

### 1. Instalare

Din surse (ai nevoie de Go 1.24+ și git 2.25+):

```bash
git clone https://github.com/nalyk/repochat.git
cd repochat
make install          # compilează și copiază binarul în ~/.local/bin
```

sau, direct cu Go (repository-ul este privat, deci Go trebuie să aibă acces la el):

```bash
GOPRIVATE=github.com/nalyk/repochat go install github.com/nalyk/repochat@latest
```

sau, după ce există un release publicat, dintr-o arhivă de la <https://github.com/nalyk/repochat/releases>: dezarhivezi și rulezi `./repochat install`. (Deocamdată nu este publicat niciunul; un tag `v*` îl produce automat.)

### 2. Prima pornire

Intri într-un repository și rulezi `repochat`. Programul te duce prin tot ce lipsește, în ordine:

1. **Verifică git și repository-ul.** Nu descarcă și nu cere nimic până nu știe că are ce servi.
2. **Instalează `tunnel-client`**, dacă nu îl găsește. Îți arată de unde descarcă și ce verifică, apoi întreabă. Versiunea implicită este fixată, iar arhiva este comparată cu sume SHA-256 compilate în repochat.
3. **Cere două valori de la OpenAI Platform**, o singură dată, într-un singur câmp ascuns (le lipești în orice ordine):
   - ID-ul unui tunel creat la <https://platform.openai.com/settings/organization/tunnels> și atașat workspace-ului tău ChatGPT;
   - o cheie API *Restricted*, cu permisiunile Tunnels: Read + Use, de la <https://platform.openai.com/settings/organization/api-keys>.

   repochat le verifică pe loc la OpenAI și le salvează într-un fișier pe care îl poți citi doar tu.
4. **Pornește tunelul și îți arată pasul din ChatGPT**, tot o singură dată: <https://chatgpt.com/plugins> → `+` → *Add custom MCP server* → Connection: **Tunnel** → alegi tunelul → *No authentication* → *Create as a plugin* (personal) → *Install*. Numele pluginului îl alegi tu; exemplele de aici folosesc „RepoChat”, de unde `@RepoChat`.
5. **Confirmă în terminal** când a servit primul apel venit din ChatGPT. Din acel moment configurarea e gata.

Pasul din ChatGPT nu poate fi făcut de un program local: tunelul și pluginul se creează în contul tău. Restul e automat.

### 3. Zi de zi

```bash
cd alt-proiect && repochat      # Ctrl+C oprește partajarea
```

Poți porni din orice subdirector. Dacă tunelul e deja folosit de alt repochat (alt repository, alt terminal), programul te întreabă dacă îl oprește pe acela și îl preia.

Fără terminal (scripturi, CI) nu se pune nicio întrebare: setezi `CONTROL_PLANE_TUNNEL_ID` și `CONTROL_PLANE_API_KEY` și adaugi `--yes` unde vrei confirmare implicită.

## Mai multe repository-uri în paralel

Un tunel servește un singur repository la un moment dat. Ca să lucrezi în mai multe în același timp, dai fiecăruia tunelul lui:

```bash
cd ~/projects/api && repochat                    # profilul implicit, primul tunel
cd ~/projects/web && repochat --profile web      # prima dată cere tunelul și cheia pentru „web”
```

- Fiecare profil are tunelul lui, creat în OpenAI Platform, și pluginul lui în ChatGPT (de exemplu „repochat-api” și „repochat-web”). Permisiunile pentru tuneluri sunt la nivel de organizație, deci aceeași cheie API ar trebui să meargă pentru toate tunelurile ei; asta nu a fost verificat cu un cont real.
- `repochat setup --profile web --bind`, rulat în repository, leagă profilul de el: de atunci ajunge `repochat`, fără `--profile`, iar profilul refuză alt repository.
- `repochat status` le listează pe toate; `repochat stop --profile web` oprește doar unul.
- Dacă pornești al doilea repochat pe **același** tunel, el nu rulează în paralel: te întreabă dacă îl oprește pe primul și îl preia.

Verificat automat, inclusiv cu două instanțe reale `tunnel-client` pornite simultan: fiecare tunel primește răspunsuri doar din repository-ul lui. Neverificat, pentru că ține de ChatGPT: cum se comportă o singură conversație în care folosești două pluginuri cu aceleași nume de instrumente. Varianta sigură este o conversație pe plugin.

## Ce vede ChatGPT

| Vede | Nu vede |
|---|---|
| fișierele pe care git le cunoaște: urmărite și neurmărite | fișierele ignorate prin `.gitignore` |
| istoricul accesibil din `HEAD`, ramuri, ramuri remote și taguri | stash, reflog, commituri orfane |
| numărul fișierelor reținute, pe categorii | conținutul și numele fișierelor reținute |
| | directorul `.git`, fișiere din afara repository-ului, ținte de symlink |
| | conținutul submodulelor (un submodul modificat apare doar ca schimbare de commit) |
| | fișierele proprii ale repochat (configurația cu cheia API, starea, `tunnel-client`), dacă se află în repository |

**Fișiere reținute.** Fișierele care arată a secrete sunt reținute implicit: `.env`, `*.pem`, `*.key`, `id_rsa`, `terraform.tfstate`, `*.tfvars`, `credentials.json`, `secrets.yaml`, `.npmrc`, `service-account.json` și altele asemenea. Regula pe cuvinte-cheie (`token`, `secret`, `password`…) se aplică doar fișierelor de date și de configurare, deci `token.go` sau `password.service.ts` rămân vizibile. Modelul află că există fișiere reținute și câte sunt în tot repository-ul, ca să nu confunde „ascuns” cu „inexistent”. Numărul nu depinde de ce caută: un răspuns nu spune niciodată câte fișiere reținute se potrivesc cu un anumit tipar sau director, pentru că așa li s-ar putea ghici numele literă cu literă.

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
| `--no-history` | doar arborele de lucru, fără commituri vechi: se refuză și `git_diff`, iar `git_status` și `repo_overview` nu mai arată identificatori de commit |
| `--deny PATTERN` | reține în plus (sintaxă `.gitignore`, repetabil) |
| `--allow PATTERN` | servește un fișier pe care regulile implicite l-ar reține |
| `--show-withheld-names` | modelul vede numele fișierelor reținute, nu și conținutul |
| `--no-redact` | oprește mascarea valorilor |
| `--max-output-kb N` | bugetul unui răspuns, cu tot cu linia `@meta` (16–2048, implicit 32) |
| `--profile NUME` | alt tunel salvat |

Un fișier `.repochatignore` în repository (aceeași sintaxă ca `.gitignore`, citită ca git: o negație `!` nu poate readuce un fișier dintr-un director exclus) reține fișiere pentru toți cei care servesc acel repository. El poate doar să strângă: `--allow` nu trece peste el.

Ordinea regulilor: fișierele proprii ale repochat → nume nesuportat → `.repochatignore` → `--deny` → `--allow` → regulile implicite pentru secrete.

Pune tiparele între ghilimele și dă fiecăruia propriul `--deny` sau `--allow`. Un tipar nepus între ghilimele poate fi extins de shell în mai multe cuvinte, iar comenzile care primesc doar opțiuni refuză orice cuvânt în plus, cu eroare, în loc să ignore în tăcere opțiunile de după el.

**Profile.** `repochat setup --profile client-x` salvează alt tunel. `repochat setup --bind` leagă un profil de repository-ul curent: profilul e ales automat acolo și refuză orice alt repository. E util când un tunel trebuie să servească un singur proiect.

## Comenzi

| Comandă | Ce face |
|---|---|
| `repochat` | partajează repository-ul curent |
| `repochat status` / `stop` | se partajează ceva acum, din orice terminal și cu orice credențiale a fost pornit? / oprește din alt terminal |
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

**Ce garantează repochat (verificat prin teste automate):**

- Niciun instrument nu scrie și niciunul nu execută comenzi arbitrare. Modelul nu furnizează niciodată argumente brute pentru git.
- Un fișier este servit doar dacă git îl listează și politica îl admite. Symlink-urile nu sunt urmate nicăieri, nici dacă un director este înlocuit cu un symlink în timpul accesului.
- Un diff sau un commit care atinge doar fișiere reținute nu produce conținut. O listă goală de căi permise nu devine „toate căile”. Git nu intră niciodată într-un submodul: nici conținutul lui, nici programele din configurația lui nu sunt atinse.
- O revizie este un nume de ramură, tag sau commit, urmat cel mult de pași spre părinți (`^`, `~N`). Formele care caută în mesajele commiturilor (`^{/text}`, `:/text`) sunt refuzate, iar o revizie inexistentă și una neaccesibilă primesc același răspuns.
- Configurația din repository nu poate face o operație de citire să ruleze un program: filtre `clean`/`smudge`, `textconv`, drivere de diff, `fsmonitor`, `diff.external`, `GIT_EXTERNAL_DIFF`, descărcări automate printr-un remote. Fiecare caz este testat cu un control pozitiv: testul arată întâi că git simplu rulează programul.
- Configurația din repository nu poate aduce în răspunsuri conținutul unui fișier din afara lui (`mailmap.file`, `blame.ignoreRevsFile`). Textul de eroare al lui git nu ajunge niciodată la model; rămâne în jurnalul local.
- Cheia API nu apare în argumentele niciunui proces și nu ajunge la git. `tunnel-client` primește un mediu construit dintr-o listă albă și îl dă mai departe, cu tot cu cheie, serverului MCP pe care îl pornește; de aceea serverul se repornește singur, înainte să servească ceva, cu un mediu construit tot dintr-o listă albă. Procesul care citește repository-ul nu ține cheia nici în mediu, nici în memorie (verificat și cu `tunnel-client` real).
- repochat nu își servește niciodată propriile fișiere, oricare ar fi opțiunile, iar directorul personal sau `/` ca rădăcină se refuză fără `--allow-broad-root`, inclusiv pentru `repochat mcp` pornit de alt client.
- Un singur repochat pe tunel, pe acest calculator. Preluarea se face cooperativ, printr-un socket privat; niciun proces nu este semnalizat după PID. Ctrl+C oprește partajarea și când ieșirea este trimisă printr-un pipe (`repochat 2>&1 | tee jurnal`); dacă cititorul acelui pipe dispare (stderr rupt), partajarea se oprește. Asta nu garantează că cineva urmărește afișajul: cu `--quiet` sau cu stderr trimis într-un fișier nu se oprește nimic.
- Textul din repository (surse, mesaje de commit, nume de fișiere, numele directorului) și textul trimis de apelant (valorile și numele argumentelor) sunt tratate ca date nesigure: caracterele de control sunt neutralizate înainte să ajungă la model sau în terminal.

**Ce nu garantează:**

- **Nu este un sandbox.** Configurația git din repository este tratată ca nesigură pentru execuție, ca măsură suplimentară. Lista filtrelor git se recitește înaintea fiecărei operații, dar un proces local care modifică `.git/config` chiar în timpul unei comenzi, sau care înlocuiește directorul repository-ului, iese din ce poate preveni repochat. Configurația git a utilizatorului și a sistemului este considerată de încredere.
- Un tipar din `.repochatignore` pe care repochat nu îl înțelege oprește servirea, cu numărul liniei. E intenționat: un tipar sărit ar expune exact ce trebuia ascuns.
- **Mascarea nu e o garanție.** Verifică singur cu `repochat files` și `repochat call`.
- **Cine poate folosi pluginul poate citi repository-ul** cât timp rulează repochat. Creează pluginul ca *personal*. Tunelul nu știe ce conversație îl folosește: dacă treci la alt repository, conversațiile vechi îl văd pe cel nou. repochat te avertizează la schimbare.
- **Instrucțiuni ascunse în cod** (prompt injection) pot influența modelul. repochat le marchează ca date, dar decizia aparține modelului.
- `tunnel-client` deschide un port de diagnoză pe `127.0.0.1` (aleatoriu). repochat însuși nu ascultă pe rețea.
- **Dimensiunea implicită a răspunsurilor (32 KB) este provizorie.** Nu a fost măsurată în sesiuni reale ChatGPT; un argument de dimensiune per apel este planificat pentru versiunea următoare.

Jurnalul local (`repochat log`) reține doar metadate: instrument, rezultat, durată, dimensiune. Argumentele se înregistrează doar cu `--audit-args`. E un istoric pentru tine, nu o pistă de audit rezistentă la modificări.

## Depanare

Începe cu `repochat doctor`: verifică git, repository-ul, serverul MCP (într-un subproces real, în ambele versiuni de protocol), `tunnel-client`, forma credențialelor și accesul la tunel, și spune clar ce nu a putut testa.

| Simptom | Cauză probabilă |
|---|---|
| tunelul nu apare în ChatGPT | nu e atașat workspace-ului ChatGPT, sau adminul a restricționat serverele MCP personalizate |
| „OpenAI rejected the tunnel connection (HTTP 401/403/404)” | cheie greșită / fără Tunnels Read + Use / ID de tunel greșit; rulează `repochat setup` |
| instrumentele nu apar după o actualizare | în setările pluginului: *Refresh tools* |
| „does not answer” la preluare sau în `repochat status` | lansatorul a fost oprit forțat și a rămas un `tunnel-client` orfan, care încă ține tunelul; oprește acel proces și repornește |
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

**Ce a fost verificat cu binarul real `tunnel-client` (v0.0.21), fără cont OpenAI:**

- Instalatorul a descărcat arhiva oficială, a verificat suma SHA-256 fixată în cod și a instalat binarul.
- Suita de contract (`make test-contract`) a rulat clientul real contra unui control plane simulat local: clientul a acceptat linia de comandă a repochat, a pornit serverul MCP prin propriul parser de argumente, a trecut de poarta de inițializare și a transportat `initialize`, `tools/list` și apeluri de instrumente cap-coadă. Stările din terminal („running”, „connected”, „remote activity”) au venit din punctul lui real de diagnoză. Căutarea tunelului (`admin tunnels get`) și `repochat doctor` au fost verificate la fel.

**Ce nu a fost verificat și rămâne de făcut de tine:**

1. **O sesiune reală ChatGPT.** Are nevoie de un tunel și de o cheie din contul tău. Controlul plane simulat urmează documentația protocolului; serverele OpenAI pot avea reguli pe care simularea nu le are. Dacă planul tău ChatGPT nu oferă Secure MCP Tunnel pentru pluginuri personale, repochat nu poate schimba asta.
2. **macOS, folosit de un om.** CI rulează pe macOS toată suita, inclusiv prima pornire în pseudo-terminal, și trece. Nimeni nu l-a folosit încă interactiv acolo, iar suita de contract cu `tunnel-client` real a rulat doar pe Linux.

Designul și apoi implementarea au fost revizuite adversarial de Codex CLI. Revizia implementării a găsit, în mai multe runde, 20 de probleme reale; toate sunt reparate și acoperite cu teste de regresie. Transcrierea este în [`docs/design/`](docs/design/).

**Auditul din 19 noiembrie 2026.** Cu toată suita verde, un al doilea audit (trei revizii adversariale, câte una pe pachet, fiecare obligată să reproducă ce raportează pe binarul compilat) a găsit încă aproximativ 50 de defecte reale. Cele mai grave: `git_diff` arăta conținutul fișierelor reținute dintr-un submodul și rula un program din configurația acelui submodul; o linie construită anume oprea tot serverul la căutare; cheia API rămânea în mediul procesului care servește, deși documentația spunea altfel; repochat își servea propriul fișier de configurare dintr-un director personal ținut în git. Toate sunt reparate, fiecare cu un test care a picat înainte de reparație (lista este în [`CHANGELOG.md`](CHANGELOG.md)). Deciziile nesigure au fost discutate cu Codex CLI până la consens: [`docs/design/audit/`](docs/design/audit/). Concluzia agreată: funcțional, 1.0 este complet; ca maturitate este un **candidat în validare** (`v1.0.0-rc.1`), până când o sesiune reală ChatGPT și o evaluare a dimensiunii răspunsurilor sunt făcute pe versiunea reparată.

## Proiect

Cod, release-uri și probleme: <https://github.com/nalyk/repochat>. Modulul Go este `github.com/nalyk/repochat`.

## Licență

MIT. `tunnel-client` este un program separat, al OpenAI, sub licență Apache-2.0.
