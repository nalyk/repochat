# repochat

[English](README.md) · Proiect: <https://github.com/nalyk/repochat>

Discuți în ChatGPT despre repository-ul Git de pe calculatorul tău, fără să-l urci nicăieri și fără să deschizi vreun port. repochat este doar pentru citire: prin el nu se poate modifica nimic, decât dacă îl pornești cu `--write` ([modul de scriere](#modul-de-scriere-experimental), experimental și oprit implicit).

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

Sau direct cu Go:

```bash
go install github.com/nalyk/repochat@latest
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

## Modul de scriere (experimental)

Modul de scriere este oprit implicit. Fără `--write`, repochat servește cele 13 instrumente doar-citire descrise mai jos și nimic din secțiunea asta nu se aplică. Modul de scriere trece testele automate, iar un prim test de scriere din contul ChatGPT al proprietarului a reușit pe 25 noiembrie 2026; o sarcină cap-coadă măsurată nu a fost încă rulată.

```bash
repochat --write                    # partajează și lasă ChatGPT să modifice fișiere într-o sesiune
repochat --write --new-session      # pornește o sesiune nouă în loc s-o reia pe cea deschisă
repochat --write --confirm-merge    # finish_session nu face merge; îl faci tu, cu repochat session finish
repochat session                    # din orice terminal: sesiunea deschisă, checkpointurile, fișierele schimbate
repochat session undo <checkpoint>  # readuce un checkpoint ca unul nou, după o întrebare [--yes]
repochat session finish             # faci merge tu, după o întrebare [--squash] [--message TEXT] [--yes]
repochat session diff               # ce ar aduce finish, sau un singur checkpoint [--checkpoint ID] [--stat]
repochat session abandon            # șterge spațiul de lucru; ramura rămâne [--delete-branch]
```

ChatGPT păstrează lista de instrumente pe care a încărcat-o mai devreme. După ce pornești prima dată cu `--write`, și din nou după ce revii la `repochat` simplu, deschide setările pluginului în ChatGPT și apasă Refresh tools. Altfel cele nouă instrumente de scriere nu apar, sau rămân în listă după ce au dispărut. Același lucru e valabil după o actualizare care adaugă parametri, ca `operation_id` și `expected_checkpoint` de mai jos. ChatGPT cere confirmare înainte să apeleze un instrument care nu este doar-citire și poate ține minte răspunsul tău pentru restul conversației, separat pentru fiecare instrument.

`--write` nu se poate combina cu `--no-history`. `repochat mcp --write` face același lucru pentru alți clienți MCP, iar `repochat tools --write` și `repochat call --write` arată și rulează instrumentele de scriere din terminal.

### Ce este o sesiune

O sesiune este a doua copie a repository-ului, făcută cu `git worktree` din commitul pe care se află checkout-ul tău, pe o ramură nouă numită `repochat/<dată>-<id>`. Stă în directorul de stare al repochat (`~/.local/state/repochat/sessions/`), nu în repository-ul tău. Modificările tale necomise nu sunt în ea; bannerul îți spune asta când ai așa ceva.

Cât timp o sesiune e deschisă, toate instrumentele lucrează pe copia aceea, inclusiv cele 13 de citire. ChatGPT își vede propriile modificări și nu vede checkout-ul tău. Fișierele pe care politica de expunere le reține nici nu sunt scrise în copie.

Fiecare schimbare aplicată de ChatGPT devine un commit pe ramura sesiunii, numit checkpoint. Nimic nu ajunge pe ramura ta până când `finish_session` nu face merge sesiunii, ca fast-forward sau ca un singur commit comprimat (squash). Poți să te uiți oricând la ramura sesiunii cu git obișnuit și să faci merge de mână în loc. Există o singură sesiune deschisă pe repository: dacă pornești din nou `repochat --write`, o reia. La reluare, copia sesiunii este readusă la ultimul ei checkpoint: tot ce a fost schimbat de mână în directorul acela și nu este un commit pe ramura sesiunii se pierde. Lucrează în checkout-ul tău, nu în copie.

Într-o sesiune, fiecare linie pe care o citește modelul poartă numărul ei și un hash scurt, de forma `12:abc|`. O modificare numește aceste ancore în loc să citeze textul vechi. Dacă fișierul s-a schimbat după ce l-a citit modelul, ancora nu se mai potrivește și modificarea este refuzată. Ancorele unui interval îi verifică doar prima și ultima linie, așa că o înlocuire sau o ștergere de trei sau mai multe linii trebuie să poarte și `file_tag`-ul fișierului: un hash scurt al întregului fișier așa cum i-a fost arătat modelului, dat de `read_file` și de fiecare rezultat al lui `apply_changes`. Și ștergerea unui fișier îl cere.

### Cele nouă instrumente

| Instrument | Rol |
|---|---|
| `start_session` | deschide o sesiune sau o reia pe cea deschisă; `fresh` pornește alta și păstrează ramura veche |
| `session_status` | ramura, punctul de plecare, checkpointurile, fișierele schimbate, dacă ramura ta s-a mișcat, fișierele schimbate care rulează mai târziu pe calculatorul tău (doar citire) |
| `propose_changes` | verifică un set de modificări și arată diff-ul pe care l-ar produce, fără să scrie nimic (doar citire) |
| `apply_changes` | aplică un set de modificări pe oricâte fișiere, complet sau deloc, ca un singur checkpoint |
| `list_checkpoints` | checkpointurile, cel mai nou primul (doar citire) |
| `undo_to_checkpoint` | readuce fișierele unui checkpoint anterior ca un checkpoint nou; istoricul păstrează tot |
| `update_session` | aduce în sesiune commiturile noi de pe ramura ta; un conflict nu schimbă nimic |
| `finish_session` | face merge sesiunii în ramura ta, în checkout-ul tău, apoi o închide (marcat distructiv) |
| `abandon_session` | închide sesiunea fără merge; ramura rămâne, dacă nu se cere altfel (marcat distructiv) |

Un set de modificări este o listă de operații: înlocuire, inserare sau ștergere de linii după ancoră; creare, ștergere sau mutare a unui fișier întreg. Se pot modifica doar fișiere text UTF-8. Fișierele Go și JSON trebuie să rămână valide sintactic după modificare; alte limbaje nu sunt verificate.

Un set de modificări poate încheia un fișier `.go` cu `{"op":"format","path":"x.go"}`. Operația formatează fișierul cu gofmt, prin pachetul `go/format` al lui Go și niciodată printr-un subproces, după celelalte operații pe acel fișier, iar rezultatul dă ancore noi. E refuzată pentru un fișier cu sfârșituri de linie CRLF sau cu marcaj de ordine a octeților (BOM), pentru un fișier cu o linie cu valoare mascată (înainte sau după formatare), pentru un fișier pe care același set îl creează, îl șterge sau îl mută, pentru un al doilea format al aceluiași fișier și când setul doar formatează un fișier deja formatat.

#### Reîncercări și apeluri depășite

- **`operation_id`** este un parametru opțional al lui `start_session`, `apply_changes`, `undo_to_checkpoint`, `update_session`, `finish_session` și `abandon_session`: de la 8 la 64 de caractere, litere, cifre, `.`, `_` sau `-`. Dacă un rezultat nu ajunge, modelul apelează din nou cu același id. Un apel repetat cu același id și exact aceleași argumente este raportat, nu repetat. Același id cu alte argumente este refuzat. Pentru un apel care face un checkpoint, înregistrarea este un trailer al commitului, `Repochat-Operation: <id> <digest>`. Pornirea, încheierea și abandonarea țin chitanțe în `operations.json`, în directorul de stare al repochat pentru acel repository, lângă `session.json`: se păstrează ultimele 64, iar chitanța unei încheieri care a început merge-ul nu se șterge niciodată. O chitanță răspunde unei reîncercări doar cât timp nu e deschisă altă sesiune: un id folosit din nou într-o sesiune ulterioară este refuzat, niciodată reluat în ea. O încheiere care a început merge-ul și nu și-a raportat rezultatul este refuzată la reîncercare, iar tu îți verifici checkout-ul de mână. Un `operations.json` deteriorat refuză orice apel care poartă un `operation_id`.
- **`expected_checkpoint`** are de la 7 la 64 de caractere hexazecimale, un prefix al checkpointului curent al sesiunii. Este opțional la `apply_changes`, `undo_to_checkpoint` și `update_session` și obligatoriu la `finish_session`, unde îl impune handlerul. Dacă sesiunea a avansat între timp, apelul este refuzat și nu se schimbă nimic.
- Rezultatul unei sesiuni încheiate numește revizia adusă: capul sesiunii și commitul de la care a pornit sesiunea. Dă comanda pentru diff-ul complet, `git diff <înainte> <commit>`, de unde era ramura ta înainte de merge până unde e acum; merge în checkout-ul tău după un fast-forward și după un squash. `repochat session diff` cere o sesiune deschisă, deci rezultatul nu trimite la el. Spune și: „No checks were run: repochat cannot run commands yet.”
- `repochat session diff` arată toată sesiunea așa cum ar aduce-o `finish_session`; `--checkpoint ID` arată un singur checkpoint, iar `--stat` doar rezumatul pe fișiere. Ieșirea are o limită; când e tăiată, spune asta și dă comanda git care arată restul.
- `repochat session undo <checkpoint>` întreabă înainte să acționeze (`--yes` sare peste întrebare). Compară opțiunile cu care rulează această comandă (opțiunile de expunere, `.repochatignore`, configurația) cu cele cu care a fost adusă sesiunea la zi ultima dată, de obicei de partajare; partajarea nu trebuie să ruleze. Dacă diferă, e refuzat: rulează-l cu aceleași opțiuni ca partajarea. După ce trece această verificare, un undo la checkpointul curent nu face nimic.

### Ce garantează

Fiecare punct e acoperit de teste automate. Unul dintre ele trece fiecare instrument de scriere prin căi și valori ostile (`..`, căi absolute, `.git` cu orice majuscule, căi protejate și reținute, symlink-uri comise în repository sau plantate în copia sesiunii) împotriva unor fișiere-martor puse în fiecare loc pe care o scriere nu are voie să-l atingă, și verifică după fiecare apel că niciun martor nu s-a schimbat.

- Scrierile au loc doar în copia sesiunii și în evidența git a ramurii ei. Singura excepție este merge-ul în ramura ta, în checkout-ul tău, când se apelează `finish_session`.
- În afară de acel merge, nicio scriere nu ajunge la o cale protejată, la un fișier reținut sau la ceva din afara copiei sesiunii, nici prin symlink-uri înlocuite în timpul apelului. Merge-ul aduce pe ramura ta ce a schimbat sesiunea, iar o sesiune nu poate schimba o cale protejată sau reținută. Căile protejate sunt `.git` cu tot ce e în el, definițiile de integrare continuă (`.github/workflows`, `.gitlab-ci.yml` și echivalentele altor servicii), directoarele de hookuri (`.husky`, `.githooks`), `.gitmodules`, `.gitattributes` și `.repochatignore`. Fișierele reținute sunt cele din [Ce vede ChatGPT](#ce-vede-chatgpt); `.repochatignore` din checkout-ul tău contează așa cum e pe disc, comis sau nu.
- Un set de modificări se aplică complet sau deloc.
- Un apel repetat cu același operation_id nu se aplică de două ori: în sesiunea deschisă pentru apelurile care fac checkpointuri și în cele mai noi 64 de chitanțe pentru pornire, încheiere și abandonare. Un id dintr-o sesiune anterioară este refuzat sau negăsit, niciodată reluat în cea nouă.
- O modificare făcută pe un conținut depășit este refuzată: ancorele răspund pentru liniile pe care le numește o modificare, iar `file_tag` pentru liniile dintre capetele unui interval mai lung și pentru un fișier șters. Fiecare fișier este recitit chiar înainte de a fi scris: unul găsit schimbat de la verificare nu este suprascris, iar setul este retras.
- O linie care conține o valoare mascată nu poate fi schimbată sau ștearsă. Contează fiecare linie a unui bloc de cheie privată, de la linia BEGIN până la linia END, și nimic nu poate fi inserat în interiorul lui; o modificare după care o linie mascată ar fi arătată altfel este refuzată în întregime. Modelului i se spune că trebuie s-o modifici tu, de mână. Cu `--no-redact` nu se maschează nimic și nu se blochează nimic.
- Niciun program configurat de repository nu rulează la niciun pas, inclusiv la merge-ul final: niciun hook, filtru, driver de merge sau program de semnare. De aceea commiturile sesiunii nu sunt semnate. Dacă configurația ta git cere commituri semnate, `finish_session` refuză și spune cum faci merge de mână. Refuză la fel cât timp `branch.<nume>.mergeOptions` este setat pentru ramura ta: git adaugă acele opțiuni la fiecare merge în ea, iar ele pot face dintr-un fast-forward un stash, un squash sau un commit de merge semnat.
- `finish_session` refuză, fără să schimbe ceva, când checkout-ul tău e pe altă ramură, când ramura ta s-a mișcat (atunci întâi `update_session`) și când merge-ul ar suprascrie modificările tale necomise, inclusiv fișierele ignorate. repochat verifică asta înainte de merge, iar git verifică din nou în timpul lui, cu instrucțiunea să nu înlocuiască nici fișierele ignorate. Rămâne clipa dintre verificarea unui fișier de către git și scrierea lui: un fișier pe care îl creezi sau îl salvezi exact atunci poate fi înlocuit.
- Fără `--write`, instrumentele, descrierile lor și rezultatele lor sunt exact cele din modul de citire.

### Ce nu garantează

- Că o modificare e corectă sau inofensivă. ChatGPT poate ține minte aprobarea ta pentru toată conversația. După aceea, textul din repository (un comentariu, un README, un issue lipit în chat) poate împinge modelul spre modificări pe care nu le-a revizuit nimeni. Până la un merge, paguba rămâne pe ramura sesiunii, deci citește diff-ul înainte de merge: `repochat session` listează fișierele, iar `git diff <ramura ta>...<ramura sesiunii>` le arată.
- Că, după merge, codul e sigur de rulat. Fișierele de build, scripturile din pachete, scripturile shell și fișierele de containere rulează mai târziu pe calculatorul tău. `finish_session` și `repochat session` le listează pe cele schimbate de sesiune; atât poate face repochat pentru ele.
- Că o sesiune aparține unei singure conversații. Toate chaturile care pot folosi pluginul lucrează în aceeași sesiune deschisă.
- Că un merge întrerupt la jumătate îți lasă checkout-ul curat. Merge-ul nu se oprește când apelul e anulat sau expiră, dar un merge care nu s-a terminat după zece minute este oprit, iar dacă git eșuează sau este oprit la mijloc, rezultatul spune că checkout-ul poate fi actualizat parțial și tipărește comenzile care îl readuc la starea dinainte sau termină merge-ul. Rulează-le exact cum sunt tipărite, în ordinea dată. Întoarcerea readuce fiecare fișier al merge-ului la ce era înainte: dacă ai modificat unul dintre ele în timpul merge-ului, salvează-ți întâi o copie.
- Că o comandă pe care repochat ți-o afișează ca s-o rulezi tu se poartă ca ale lui. Când îți spune cum să faci de mână merge-ul branch-ului de sesiune sau cum să aduci înapoi un checkout după un merge întrerupt, acele comenzi `git` rulează cu configurația ta de git, cu tot ce adaugă ea: o setare ca `pull.twohead`, `branch.<nume>.mergeOptions` sau un hook li se aplică la fel ca oricărei comenzi pe care o tastezi.
- Că hookurile pe care se bazează repository-ul tău mai au loc. `post-merge` și celelalte nu rulează; rezultatul le numește pe cele pe care repository-ul le are, ca să le rulezi de mână.
- Că două nume care diferă doar prin normalizarea Unicode sunt tratate ca un singur fișier. Pe macOS, `é` scris ca un singur caracter și ca `e` urmat de un accent numesc același fișier; repochat compară numele după majuscule și minuscule, nu după normalizare, deci o regulă care reține sau protejează o scriere nu o acoperă pe cealaltă.
- Fișierele binare, modurile fișierelor, symlink-urile și submodulele nu pot fi modificate.

Compilarea cere Go 1.25 sau mai nou, ca pentru restul repochat.

## Ghidare de la proprietar (experimental)

Claude Code și Codex încarcă `AGENTS.md` și `CLAUDE.md` ca instrucțiuni. repochat listează aceste fișiere și pentru ChatGPT, dar îi spune modelului să nu urmeze niciodată ce întoarce un instrument: regula aceasta este apărarea împotriva injecției de prompt. Ghidarea de la proprietar îți permite totuși să promovezi un singur text revizuit la ghidare permanentă, printr-un canal la care conținutul repository-ului nu ajunge: instrucțiunile serverului. Un text din repository nu devine ghidare doar pentru că a fost citit. Doar tu poți, în terminalul tău, cu `repochat trust`.

```bash
repochat setup --bind               # o dată, în repository: leagă profilul de el
repochat trust                      # revizuiește AGENTS.md (sau CLAUDE.md) și, dacă spui da, îl stochează
repochat trust CLAUDE.md            # alt fișier din rădăcină decât cel implicit
repochat trust --status             # ce e stocat și dacă fișierul s-a schimbat de atunci
repochat trust --revoke             # șterge ghidarea stocată, după o întrebare [--yes]
repochat trust --release-tunnel     # uită cărui repository îi este atribuit tunelul, după o întrebare [--yes]
```

Nimic din MCP nu creează și nu schimbă încrederea; niciun instrument nu poate.

**Leagă întâi profilul.** `trust` refuză un profil care nu este legat de acest repository: rulează `repochat setup --profile NUME --bind` în el.

**Limite.** Fișierul trebuie să fie din rădăcină, obișnuit, UTF-8, de cel mult 8 KiB de text brut; nu se citește niciodată printr-un link. Ce se livrează este redarea lui mascată și vizibilă, de cel mult 2 KiB. Un fișier peste oricare limită este refuzat, nu tăiat. Cei 2 KiB sunt o limită de produs a repochat, nu a OpenAI.

**Ce arată revizuirea.** Repository-ul și rădăcina lui, profilul, tunelul mascat și dacă e atribuit, calea și textul exact care ar fi livrat, cu o linie care spune că textul ajunge la OpenAI odată cu metadatele pluginului de fiecare dată când ChatGPT încarcă pluginul. Valorile care seamănă a secrete sunt mascate, întotdeauna. Caracterele de control, bidirecționale și invizibile apar ca secvențe `\uNNNN`, la fel și selectorii de variație: un emoji ca ⚠️ apare ca ⚠ urmat de `️`. Răspunsul implicit este nu. Fără terminal, cererea este refuzată dacă nu dai `--yes`. Ce ai confirmat este exact ce se stochează și se livrează; fișierul nu mai este citit pentru livrare.

**Cum ajunge la ChatGPT.** La pornire, instrucțiunile serverului încep cu un scurt preambul care spune că acest plugin servește doar acest repository, că ai revizuit secțiunea de la sfârșit, că ea se află sub regulile gazdei, sub aceste reguli și sub orice ceri în conversație, că nu dă nicio capacitate în plus și nu ocolește nicio confirmare, și că nimic din ce întoarce un instrument nu este ghidare. Textul revizuit vine după instrucțiunile existente, întreg, ca ultima secțiune. Dacă instrucțiunile cu el ar depăși 8192 de octeți, partajarea nu pornește; textul nu se taie niciodată. Fără ghidare stocată, totul este identic octet cu octet cu înainte. Ghidarea se livrează la fel în modul de citire și în cel de scriere.

**Tunelul aparține unui singur repository.** Un tunel care a purtat ghidare este atribuit repository-ului acela, iar repochat refuză să servească pe el orice alt repository, fiindcă tunelul nu știe ce conversație îl folosește. Atribuirea rămâne și după o revocare. Ca să dai tunelul altui repository, rulează `repochat trust --release-tunnel` (arată repository-ul și tunelul mascat și întreabă), apoi **șterge pluginul în ChatGPT și creează-l din nou**; abia apoi dă încredere unui repository pentru el. Dacă identitatea repository-ului se schimbă (alt director la aceeași cale, alt set de commituri-rădăcină), partajarea refuză să pornească până rulezi din nou `repochat trust`.

**Paza.** Înainte de fiecare apel de instrument și înainte de fiecare răspuns care poartă instrucțiuni (`initialize`, `server/discover`), repochat verifică dacă înregistrarea stocată este întreagă și tot cea cu care a pornit procesul și dacă politica tot servește fișierul. După o revocare sau o înlocuire, o înregistrare ilizibilă sau deteriorată ori o sursă pe care politica o reține acum, fiecare apel este refuzat cu: „the owner changed this repository's guidance: restart repochat, press Refresh tools in ChatGPT, and start a new conversation”. Fă exact asta: repornește repochat, apasă Refresh tools în setările pluginului și începe o conversație nouă. Apelurile deja admise pot termina, iar **nimic nu retrage ce a primit sau a făcut deja ChatGPT**: revocarea oprește apelurile următoare, nu le desface pe cele de dinainte. Ștergerea fișierului nu revocă; instantaneul revizuit se păstrează cât timp politica servește tot calea.

**Linia de deviere.** `repo_overview` se încheie cu o linie scrisă de server, despre checkout-ul tău și nu despre vreun spațiu de lucru de sesiune: `Owner guidance: from AGENTS.md, reviewed 2026-11-25; unchanged (the reviewed version is in the instructions)`. Starea este `unchanged` (neschimbat), `changed` (hash diferit), `removed` (doar când fișierul nu mai există cu certitudine) sau `could not be compared` (o eroare de permisiune sau de I/O, ori un tip de fișier nesigur de citit). Terminalul primește o singură înștiințare pe proces, prima dată când starea nu este `unchanged`. `repochat trust --status` arată aceeași stare. Linia este doar informare: ChatGPT păstrează textul revizuit până revizuiești din nou fișierul.

**Modelul de amenințare.** Autorii repository-ului și tot ce controlează un model (argumente, nume de fișiere, commituri, rezultate de căutare, text copiat între ele) sunt adversari. Contul tău, terminalul tău și binarul repochat sunt de încredere. Un marcaj sau un titlu dintr-un rezultat de instrument nu poate autentifica nimic, de aceea nu există unul: singura ghidare este segmentul pe care l-ai revizuit, din instrucțiuni. **O strămoșie comună nu autentifică un proiect**: două repository-uri cu aceleași commituri-rădăcină (un fork, o clonă) nu sunt același proiect pentru încrederea repochat, de aceea legătura ține și calea, dispozitivul și inode-ul checkout-ului.

**Experimental.** Dacă ChatGPT urmează ghidarea și refuză să trateze copiile ei drept ghidare nu a fost măsurat. Funcția rămâne experimentală până când protocolul din [`docs/guidance-test.md`](docs/guidance-test.md) trece cu praguri fixate. Designul: [`docs/superpowers/specs/2026-11-25-owner-guidance-design.md`](docs/superpowers/specs/2026-11-25-owner-guidance-design.md).

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
| `repochat session` | sesiunea deschisă din modul de scriere; `diff`, `undo`, `finish`, `abandon` |
| `repochat trust` | revizuiește `AGENTS.md` / `CLAUDE.md` și îl dă ChatGPT ca ghidare permanentă; `--status`, `--revoke`, `--release-tunnel` |
| `repochat mcp` | serverul MCP propriu-zis (stdio) |
| `repochat mcp-config` | configurare pentru alți clienți MCP |
| `repochat install`, `install-helper` | instalează binarul, respectiv `tunnel-client` |
| `repochat help chatgpt` | pașii din ChatGPT |

## Instrumentele pe care le primește modelul

Toate cele 13 sunt marcate `readOnlyHint`, deci ChatGPT nu cere confirmare la fiecare apel. Modul de scriere mai adaugă nouă, descrise la [modul de scriere](#modul-de-scriere-experimental).

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

- Fără `--write`, niciun instrument nu scrie. Niciun instrument nu execută comenzi arbitrare, iar modelul nu furnizează niciodată argumente brute pentru git. Ce adaugă modul de scriere e descris la [modul de scriere](#modul-de-scriere-experimental).
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

Jurnalul local (`repochat log`) reține doar metadate. Argumentele se înregistrează doar cu `--audit-args`. În modul de scriere, fiecare set de modificări aplicat, fiecare undo, actualizare și merge se înregistrează cu checkpointul, numărul de linii și numele fișierelor atinse (primele 20, apoi câte mai sunt), niciodată cu conținutul lor. Numele acestor fișiere se înregistrează și fără `--audit-args`. E un istoric pentru tine și poate fi modificat de oricine are contul tău.

Ai găsit o breșă? Raporteaz-o privat; [`SECURITY.md`](SECURITY.md) (în engleză) spune cum și ce se pune la socoteală.

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
4. O sarcină măsurată în modul de scriere, dintr-o sesiune reală ChatGPT: confirmări, apeluri, latență, alocarea Codex. Un prim test de scriere din contul proprietarului a reușit pe 25 noiembrie 2026, deci acel cont poate apela instrumente care nu sunt doar-citire; altceva nu s-a măsurat încă.

## Licență

MIT. `tunnel-client` este un program separat, al OpenAI, sub licență Apache-2.0.
