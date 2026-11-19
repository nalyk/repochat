# repochat

[English](README.md) · Proiect: <https://github.com/nalyk/repochat>

Discuți în ChatGPT despre repository-ul Git de pe calculatorul tău, fără să-l urci nicăieri și fără să deschizi vreun port. repochat este doar pentru citire: prin el nu se poate modifica nimic.

```bash
cd ~/projects/proiectul-meu
repochat
```

Apoi, în ChatGPT: `@RepoChat ce face AuthenticationService și unde e folosit?`

Este un singur binar Go, fără dependențe externe. Vorbește MCP și ajunge la ChatGPT prin [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels), cu `tunnel-client`, programul OpenAI, pe care repochat îl instalează singur la prima pornire.

> Citește asta înainte de orice. Tot ce citește ChatGPT din repository ajunge la OpenAI. „Local” înseamnă aici că nu urci nimic dinainte și că legătura pleacă de pe calculatorul tău. Nu înseamnă că datele rămân pe el. Folosește repochat doar cu cod pe care ai voie să-l arăți.

## De ce există

L-am scris din cauza felului în care contorizează OpenAI.

Codex din terminal, din aplicația desktop și din IDE consumă toate din aceeași alocare. [Pagina de prețuri](https://learn.chatgpt.com/docs/pricing) a OpenAI o spune direct: „ChatGPT Work and Codex share pricing, credits, and usage limits.” Pe Plus alocarea este o fereastră de cinci ore, cu o limită săptămânală peste ea, iar estimarea pentru modelul implicit e undeva între 15 și 160 de mesaje locale pe fereastră, după cât de greu e fiecare. Planurile Pro nu au fereastra de cinci ore, dar limita săptămânală rămâne.

E în regulă cât timp agentul modifică cod. E o afacere proastă pentru cealaltă jumătate a muncii, adică discuția: „de ce arată modulul ăsta așa”, „ce s-ar strica dacă schimb asta”, „citește fișierele astea patru și spune-mi ce ai face”. Un agent răspunde la asemenea întrebări trăgând jumătate de repository în context, iar fiecare întrebare se scade din același buget de care am nevoie mai târziu pentru lucrul propriu-zis. Mi s-a întâmplat de mai multe ori să rămân fără el la mijlocul săptămânii, după ce în mare parte doar pusesem întrebări.

ChatGPT obișnuit, chatul din browser, este altă suprafață. Aceeași pagină de prețuri spune despre modelele agentice că sunt disponibile „in Work and Codex, not Chat”, iar Chat nu apare în alocarea comună pe care o descrie. Tot în Chat îmi fac și documentarea: căutare pe web, discuții lungi, idei pe jumătate formate. Ce nu poate Chat este să-mi vadă codul, dacă nu i-l lipesc eu sau nu-l urc undeva unde ajunge.

repochat acoperă golul ăsta. Repository-ul rămâne unde este, ChatGPT cere fișierele de care are nevoie, iar conversația are loc în Chat, nu într-o sesiune de agent. Planific și mă documentez în browser, cu modelul uitându-se la codul real, și păstrez alocarea Codex pentru sesiunile care chiar schimbă ceva.

Trei rezerve, ca să fie limpede ce știu și ce nu:

- Nu am măsurat cum se contorizează apelurile făcute de un plugin față de limitele proprii ale Chat, iar OpenAI nu o spune explicit. Ghiduri neoficiale susțin că socoteala dintre ChatGPT și Codex se schimbă din nou curând. Uită-te în [panoul tău de utilizare](https://chatgpt.com/codex/settings/usage), nu te baza pe paragraful ăsta.
- OpenAI documentează pluginurile MCP personalizate pentru ChatGPT în browser. Nu am încercat aplicațiile ChatGPT de mobil sau de desktop și nu afirm nimic despre ele.
- Secure MCP Tunnel a fost anunțat pentru clienții enterprise. Dacă planul tău nu îl oferă pentru pluginuri personale, repochat nu poate schimba asta.

Dacă lucrezi deja doar în Codex sau în Claude Code și nu dai niciodată de limită, probabil că nu ai nevoie de el.

## Pornire rapidă

### 1. Instalare

Din surse (Go 1.25+ și git 2.25+):

```bash
git clone https://github.com/nalyk/repochat.git
cd repochat
make install          # compilează și copiază binarul în ~/.local/bin
```

Sau direct cu Go (repository-ul este privat, deci Go trebuie să aibă acces la el):

```bash
GOPRIVATE=github.com/nalyk/repochat go install github.com/nalyk/repochat@latest
```

Încă nu există niciun release publicat. Un tag `v*` pe `main` produce unul.

### 2. Prima pornire

Intri într-un repository și rulezi `repochat`. Te duce prin ce lipsește, în ordinea asta:

1. Verifică întâi git și repository-ul. Nu descarcă și nu cere nimic până nu știe că are ce servi.
2. Instalează `tunnel-client`, dacă nu îl găsește. Îți arată de unde descarcă și ce verifică, apoi întreabă. Versiunea este fixată, iar arhiva e comparată cu sume SHA-256 compilate în repochat.
3. Cere o singură dată două valori de la OpenAI Platform, într-un singur câmp ascuns (le lipești în orice ordine):
   - ID-ul unui tunel creat la <https://platform.openai.com/settings/organization/tunnels> și atașat workspace-ului tău ChatGPT;
   - o cheie API Restricted, cu Tunnels Read + Use, de la <https://platform.openai.com/settings/organization/api-keys>.

   Le verifică la OpenAI și le salvează într-un fișier pe care îl poți citi doar tu.
4. Pornește tunelul și îți arată singurul pas care se face în ChatGPT: <https://chatgpt.com/plugins> → `+` → Add custom MCP server → Connection: Tunnel → alegi tunelul → No authentication → Create as a plugin (personal) → Install. Numele pluginului îl alegi tu; exemplele de aici folosesc „RepoChat”.
5. Confirmă în terminal când a servit primul apel venit din ChatGPT.

Pasul din ChatGPT nu îl poate face niciun program local. Tunelul și pluginul sunt în contul tău.

### 3. Zi de zi

```bash
cd alt-proiect && repochat      # Ctrl+C oprește partajarea
```

Poți porni din orice subdirector. Dacă tunelul e deja folosit de alt repochat, te întreabă dacă îl oprește pe acela și îl preia.

Fără terminal (scripturi, CI) nu întreabă nimic: setezi `CONTROL_PLANE_TUNNEL_ID` și `CONTROL_PLANE_API_KEY` și adaugi `--yes` unde vrei confirmarea dată din start.

## Mai multe repository-uri în paralel

Un tunel servește un singur repository la un moment dat. Ca să lucrezi în mai multe deodată, îi dai fiecăruia tunelul lui:

```bash
cd ~/projects/api && repochat                    # profilul implicit, primul tunel
cd ~/projects/web && repochat --profile web      # cere o dată tunelul și cheia pentru „web”
```

- Fiecare profil are tunelul lui și pluginul lui în ChatGPT. Permisiunile pentru tuneluri sunt la nivel de organizație, deci aceeași cheie API ar trebui să meargă pentru toate; asta nu a fost verificat cu un cont real.
- `repochat setup --profile web --bind`, rulat în repository, leagă profilul de el. De atunci ajunge `repochat` simplu acolo, iar profilul refuză orice alt repository.
- `repochat status` listează toate partajările de pe calculator. `repochat stop --profile web` oprește una.

Cum se poartă o singură conversație cu două pluginuri care au aceleași nume de instrumente ține de ChatGPT și nu a fost testat. Varianta sigură este o conversație pe plugin.

Există un design pentru un singur plugin care servește mai multe repository-uri, în [`docs/superpowers/`](docs/superpowers/). Nimic din el nu este construit.

## Ce vede ChatGPT

| Vede | Nu vede |
|---|---|
| fișierele pe care git le cunoaște: urmărite și neurmărite | fișierele ignorate prin `.gitignore` |
| istoricul accesibil din `HEAD`, ramuri, ramuri remote și taguri | stash, reflog, commituri orfane |
| câte fișiere sunt reținute în tot repository-ul, pe categorii | numele și conținutul fișierelor reținute |
| | directorul `.git`, fișiere din afara repository-ului, ținte de symlink |
| | conținutul submodulelor (un submodul modificat apare doar ca schimbare de commit) |
| | fișierele proprii ale repochat (configurația cu cheia API, starea, `tunnel-client`) |

Fișierele care arată a secrete sunt reținute implicit: `.env`, `*.pem`, `*.key`, `id_rsa`, `terraform.tfstate`, `*.tfvars`, `credentials.json`, `secrets.yaml`, `.npmrc`, `service-account.json` și altele asemenea. Regula pe cuvinte-cheie (`token`, `secret`, `password`…) se aplică doar fișierelor de date și de configurare, deci `token.go` rămâne vizibil. Modelului i se spune că există fișiere reținute și câte sunt în total. Numărul nu depinde niciodată de ce caută, pentru că o numărătoare pe interogare i-ar permite să ghicească numele literă cu literă.

Valorile care arată a secrete (chei AWS, tokenuri GitHub, GitLab și Slack, chei OpenAI, Anthropic, Google și Stripe, JWT, parole în URL-uri, chei private PEM) apar ca `[REDACTED:tip]`. Căutarea lucrează pe textul deja mascat. Mascarea se face cât se poate: un secret într-un format pe care repochat nu îl cunoaște nu este prins.

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
| `--no-history` | doar arborele de lucru: instrumentele de istoric și `git_diff` sunt refuzate, iar `git_status` și `repo_overview` nu arată identificatori de commit |
| `--deny TIPAR` | reține în plus (sintaxă `.gitignore`, repetabil) |
| `--allow TIPAR` | servește un fișier pe care regulile implicite l-ar reține |
| `--show-withheld-names` | modelul vede numele fișierelor reținute, niciodată conținutul |
| `--no-redact` | oprește mascarea valorilor |
| `--max-output-kb N` | dimensiunea unui răspuns, cu tot cu linia `@meta` (16–2048, implicit 32) |
| `--profile NUME` | alt tunel salvat |

Un fișier `.repochatignore` în repository reține fișiere pentru oricine îl servește, cu aceeași sintaxă ca `.gitignore`. Ce înseamnă o regulă hotărăște git-ul tău: repochat întreabă `git check-ignore` într-un repository gol, izolat de configurația ta, a sistemului și a repository-ului servit. Poți verifica o regulă cu aceeași comandă. Numele se compară exact, inclusiv majusculele. Dacă git nu poate fi întrebat, nu se servește nimic. `.repochatignore` poate doar să strângă; `--allow` nu trece peste el.

Ordinea regulilor: fișierele proprii ale repochat → nume nesuportat → `.repochatignore` → `--deny` → `--allow` → regulile implicite pentru secrete.

Pune tiparele între ghilimele și dă fiecăruia propriul `--deny` sau `--allow`. Un tipar fără ghilimele poate fi extins de shell în mai multe cuvinte, iar comenzile care primesc doar opțiuni se opresc cu eroare la un cuvânt în plus, în loc să ignore în tăcere opțiunile de după el.

## Comenzi

| Comandă | Ce face |
|---|---|
| `repochat` | partajează repository-ul curent |
| `repochat status` / `stop` | se partajează ceva acum, din orice terminal? / oprește din alt terminal |
| `repochat doctor` | verifică separat fiecare parte și spune ce nu a putut testa |
| `repochat setup` | introduce sau schimbă tunelul și cheia; `--list`, `--remove`, `--bind` |
| `repochat files`, `tools`, `call` | ce se servește, ce instrumente primește modelul, rulezi tu un instrument |
| `repochat log` | activitatea recentă (ce instrument, când, cât a durat) |
| `repochat mcp` | serverul MCP propriu-zis (stdio) |
| `repochat mcp-config` | configurare pentru alți clienți MCP |
| `repochat install`, `install-helper` | instalează binarul, respectiv `tunnel-client` |
| `repochat help chatgpt` | pașii din ChatGPT |

## Instrumentele pe care le primește modelul

Toate cele 13 sunt marcate `readOnlyHint`, deci ChatGPT nu cere confirmare la fiecare apel.

| Instrument | Rol |
|---|---|
| `repo_overview` | ramură, HEAD, modificări necomise, structură, tipuri de fișiere, ce poate vedea sesiunea |
| `list_files`, `find_files` | navigare în arbore; găsire după nume sau tipar |
| `read_file`, `read_files` | citire cu numere de linie și indicația de continuare; și la o revizie veche |
| `search_code` | căutare literală sau RE2, cu context, filtre și cursor; spune când e incompletă |
| `file_outline` | definițiile dintr-un fișier (Go: analizor real; alte limbaje: euristic, etichetat ca atare) |
| `git_status`, `git_log`, `git_diff`, `git_show`, `git_blame`, `git_refs` | starea și istoricul |

Fiecare răspuns se termină cu o linie `@meta {json}`: dacă e complet, cursorul sau linia următoare, câte valori au fost mascate.

## Securitate

Ce garantează repochat, fiecare punct fiind acoperit de teste automate:

- Niciun instrument nu scrie și niciunul nu execută comenzi arbitrare. Modelul nu furnizează niciodată argumente brute pentru git.
- Un fișier este servit doar dacă git îl listează și politica îl admite. Symlink-urile nu sunt urmate niciodată, nici dacă un director este înlocuit cu unul în timpul accesului.
- Un diff sau un commit care atinge doar fișiere reținute nu produce conținut. Git nu intră niciodată într-un submodul.
- O revizie este un nume de ramură, tag sau commit, urmat cel mult de pași spre părinți. Formele care caută în mesajele commiturilor sunt refuzate, iar o revizie inexistentă și una neaccesibilă primesc același răspuns.
- Configurația din repository nu poate face o citire să ruleze un program (filtre, `textconv`, drivere de diff, `fsmonitor`, `diff.external`, descărcări automate). Fiecare caz e testat cu un control pozitiv care arată că git simplu chiar îl rulează.
- Textul de eroare al lui git nu ajunge niciodată la model.
- Cheia API nu apare în argumentele niciunui proces și nu ajunge la git. `tunnel-client` își dă mediul, cu tot cu cheie, serverului pe care îl pornește, așa că serverul se repornește singur cu un mediu dintr-o listă albă înainte să servească ceva.
- repochat nu își servește niciodată propriile fișiere și refuză directorul personal sau `/` ca rădăcină fără `--allow-broad-root`.
- Un singur repochat pe tunel, pe acest calculator. Preluarea se face cooperativ, printr-un socket privat; niciun proces nu este semnalizat după PID. Ctrl+C oprește partajarea și când ieșirea trece printr-un pipe.
- Textul din repository și textul trimis de apelant au caracterele de control neutralizate înainte să ajungă la model sau în terminalul tău.

Ce nu garantează:

- Nu este un sandbox. Întărirea lui git înseamnă oprirea, una câte una, a setărilor periculoase cunoscute, iar o versiune nouă de git poate aduce una pe care nu am văzut-o. Configurația git a utilizatorului și a sistemului este considerată de încredere.
- Mascarea nu e o garanție. Verifică singur cu `repochat files` și `repochat call`.
- Cine poate folosi pluginul poate citi repository-ul cât timp rulează repochat. Creează-l ca personal. Tunelul nu știe ce conversație îl folosește: dacă schimbi repository-ul, conversațiile vechi îl văd pe cel nou.
- Commiturile vechi sunt servite implicit, iar un secret șters din arborele de lucru poate fi încă în ele. Folosește `--no-history` dacă te îngrijorează.
- Instrucțiunile ascunse în cod (prompt injection) pot influența modelul. repochat marchează textul din repository ca date; ce face modelul cu el ține de model.
- Dimensiunea implicită de 32 KB a unui răspuns este provizorie și nu a fost măsurată în sesiuni reale ChatGPT.
- `tunnel-client` deschide un port de diagnoză pe `127.0.0.1`. repochat însuși nu ascultă pe nimic.

Jurnalul local (`repochat log`) reține doar metadate. Argumentele se înregistrează doar cu `--audit-args`. E un istoric pentru tine și poate fi modificat de oricine are contul tău.

## Depanare

Începe cu `repochat doctor`.

| Simptom | Cauză probabilă |
|---|---|
| tunelul nu apare în ChatGPT | nu e atașat workspace-ului ChatGPT, sau un admin a restricționat serverele MCP personalizate |
| „OpenAI rejected the tunnel connection (HTTP 401/403/404)” | cheie greșită, fără Tunnels Read + Use, sau ID de tunel greșit; rulează `repochat setup` |
| instrumentele lipsesc după o actualizare | în setările pluginului: Refresh tools |
| „does not answer” la preluare sau în `repochat status` | lansatorul a fost oprit forțat și a rămas un `tunnel-client` care încă ține tunelul; oprește acel proces |
| modelul nu găsește un fișier | e ignorat de `.gitignore` sau reținut; vezi `repochat files --withheld` |

`repochat --verbose` arată și jurnalul lui `tunnel-client`.

## Alți clienți MCP

`repochat mcp` este un server MCP obișnuit pe stdio și nu are nevoie de tunel sau de credențiale. `repochat mcp-config` tipărește configurarea pentru Claude Desktop, Claude Code, Cursor sau VS Code. Vorbește reviziile cu handshake (2024-11-05 până la 2025-11-25) și revizia fără stare 2026-07-28.

## Dezvoltare și unde se află proiectul

```bash
make check            # vet, teste cu detectorul de curse, prima pornire în pseudo-terminal
make fuzz             # fuzzing scurt pe validatorii de intrare
make dist             # arhive pentru linux/darwin × amd64/arm64, cu sume de control
make test-contract    # suita de contract cu tunnel-client REAL și un control plane simulat
```

CI rulează toată suita pe Linux și macOS, iar testele pentru accesul la repository și pentru instrumente încă o dată cu git 2.25, 2.34 și 2.43.

Ca funcții, 1.0 este complet. Ca maturitate, este un candidat în validare, iar eu nu aș pune încă în spatele lui un repository pe care nu-mi permit să-l scap. Un audit din 19 noiembrie 2026 a găsit aproximativ 50 de defecte reale în spatele unui CI verde, printre care unul care scotea fișiere reținute din submodule. Sunt reparate, fiecare cu un test care a picat înainte, iar lista e în [`CHANGELOG.md`](CHANGELOG.md). Prima variantă a uneia dintre reparații era ea însăși greșită pe un git mai nou, lucru pe care l-a prins CI. Reviziile de design și discuția de audit cu Codex CLI sunt în [`docs/design/`](docs/design/).

Ce nu e făcut încă:

1. O sesiune reală ChatGPT pe versiunea reparată. Are nevoie de un tunel și de o cheie din contul tău; control plane-ul simulat urmează protocolul documentat, iar serverele OpenAI pot avea reguli pe care el nu le are.
2. O măsurare a dimensiunii răspunsurilor în conversații reale.
3. Folosirea interactivă pe macOS. CI trece acolo, dar nimeni nu a stat în fața lui.

## Licență

MIT. `tunnel-client` este un program separat, al OpenAI, sub licență Apache-2.0.
