# Jurnal de modificări

Proiect: [`nalyk/repochat`](https://github.com/nalyk/repochat)

## Nelansat: modul de scriere (experimental)

Inversează, la cererea proprietarului, regula „doar citire pentru totdeauna” din designul 1.0, pentru un mod pornit explicit. Fără `--write` totul rămâne ca în 1.0.0: aceleași 13 instrumente, cu aceleași texte. Un prim test de scriere dintr-o sesiune reală ChatGPT a reușit pe 25 noiembrie 2026; nicio sarcină măsurată încă. Designul: [`docs/superpowers/specs/2026-11-19-write-mode-edits-design.md`](docs/superpowers/specs/2026-11-19-write-mode-edits-design.md).

- `repochat --write` (și `repochat mcp --write`): ChatGPT poate modifica fișiere într-o sesiune, adică un `git worktree` pe o ramură `repochat/<dată>-<id>`, sub directorul de stare al repochat, niciodată în checkout-ul proprietarului. `--new-session` pornește o sesiune nouă, `--confirm-merge` lasă merge-ul în seama terminalului.
- Nouă instrumente noi: `start_session`, `session_status`, `propose_changes`, `apply_changes`, `list_checkpoints`, `undo_to_checkpoint`, `update_session`, `finish_session`, `abandon_session`. Cele de citire servesc spațiul de lucru al sesiunii cât timp e deschisă.
- Liniile citite într-o sesiune poartă ancore (`12:abc|`, un hash cu cheie al liniei așa cum o vede modelul, după mascare); o modificare pe o ancoră depășită este refuzată și primește liniile actuale.
- Un set de modificări se aplică complet sau deloc și devine un checkpoint (un commit făcut cu comenzi git de nivel jos, fără hookuri și fără semnare); `undo_to_checkpoint` nu șterge nimic din istoric și nu trece înapoi peste o actualizare din ramura proprietarului.
- Refuzate: căile protejate (`.git`, definițiile de CI, directoarele de hookuri, `.gitmodules`, `.gitattributes`, `.repochatignore`), fișierele reținute (inclusiv după un `.repochatignore` necomis), symlink-urile și submodulele pe drum, fișierele cu atribut `filter`, liniile cu valori mascate, Go sau JSON invalid, marcajele de conflict.
- Fișierele reținute nu sunt scrise deloc în spațiul de lucru (sparse checkout), iar absența lor e verificată pe disc după fiecare sincronizare.
- `finish_session` face fast-forward sau un commit comprimat și refuză, fără să schimbe ceva, pe altă ramură, după ce ramura proprietarului s-a mișcat, la commituri semnate cerute și când ar suprascrie modificări necomise. Listează fișierele schimbate care rulează mai târziu pe calculatorul proprietarului; dacă git eșuează la mijlocul merge-ului, tipărește comenzile de revenire.
- `repochat session` (cu `finish` și `abandon`) arată și închide sesiunea din terminal, fără să-i atingă fișierele.
- Un test de frontieră trece fiecare instrument de scriere prin căi și valori ostile împotriva unor fișiere-martor (în checkout, în `.git`, în afara repository-ului, în fișierele reținute și protejate) și verifică după fiecare apel că nimic din afara spațiului de lucru nu s-a schimbat. A găsit un defect, reparat cu un test care a picat înainte: `finish_session` lăsa fast-forward-ul lui git să suprascrie un fișier ignorat al proprietarului aflat pe o cale pe care sesiunea o crease; acum refuză.

Corecturi după cele două revizii ale întregii ramuri (una internă, una adversarială externă), fiecare reprodusă întâi printr-un test:

- Directoarele proprii ale repochat aflate în checkout sunt reținute și în spațiul de lucru al sesiunii, nu doar în repository-ul proprietarului.
- `finish_session` refuză, fără să schimbe ceva, cât timp `branch.<nume>.mergeOptions` este setat pentru ramura de pornire: opțiunile acelea puteau lăsa marcaje de conflict și un stash în checkout (`--autostash`), puteau pregăti un squash fără să mute ramura sau puteau rula programul de semnare. Merge-ul primește și `--no-gpg-sign` și `--no-overwrite-ignore`, deci un fișier ignorat apărut după ultima verificare nu mai este înlocuit.
- Ambele merge-uri, al lui `finish_session` în checkout-ul proprietarului și al lui `update_session` în spațiul de lucru, își numesc strategia (`ort` de la git 2.34, `recursive` înainte, adică implicita lui git), așa că `pull.twohead` din configurația proprietarului nu o mai poate alege. Cu `pull.twohead=ours`, fast-forward-ul făcea un commit de merge care păstra arborele proprietarului și pierdea modificările sesiunii, iar actualizarea pierdea modificările proprietarului; cu numele unui program, actualizarea rula `git-merge-<nume>`.
- Un refuz curat al lui git la merge este raportat ca refuz, nu ca „checkout posibil actualizat parțial”: comenzile de revenire ar fi suprascris tocmai modificarea proprietarului. Mesajul pentru un merge întrerupt spune acum că revenirea suprascrie o modificare făcută în timpul merge-ului, iar revenirea nu mai șterge niciun fișier ignorat.
- Fiecare linie a unui bloc de cheie privată este blocată, de la BEGIN la END, inclusiv un marcaj de conflict aflat între liniile cheii; nu se mai poate insera nimic acolo unde blocul e deschis, iar o modificare după care o linie mascată ar fi arătată altfel este refuzată. Înainte, o inserare lângă un asemenea marcaj dezvăluia restul cheii.
- O înlocuire sau o ștergere de trei sau mai multe linii cere `file_tag`-ul actual al fișierului: ancorele verifică doar capetele intervalului, iar o linie schimbată între ele era suprascrisă nevăzută. `apply_changes` dă noul `file_tag` al fiecărui fișier atins.
- O regulă adăugată în `.repochatignore`-ul proprietarului în timp ce serverul rulează se aplică la următorul apel: fișierul nou reținut iese din spațiul de lucru și nu mai poate fi scris nici în cele trei secunde în care lista de fișiere era ținută în memorie.
- Fiecare fișier este recitit chiar înainte de a fi scris sau șters; unul schimbat între verificare și scriere nu mai este suprascris.
- Spațiul de lucru este șters doar dacă drumul înregistrat este chiar worktree-ul sesiunii (niciun symlink pe drum, iar git îl listează exact acolo): un `session.json` modificat, cu un symlink spre alt worktree, nu mai poate șterge acel worktree.
- `undo_to_checkpoint` funcționează și peste un fișier devenit director sau invers: ștergerile se fac întâi.
- Mutarea ramurii sesiunii la `apply_changes` și `update_session` nu mai depinde de anularea apelului: rezultatul se decide din locul în care se află ramura. După o actualizare a cărei sincronizare eșuează, spațiul de lucru este închis și readus la următorul apel.
- O linie `=======` singură (sublinierea unui titlu) nu mai este refuzată ca marcaj de conflict; este refuzată doar împreună cu un `<<<<<<<` sau `>>>>>>>` nou.
- Planurile păstrate pentru `apply_changes` rețin un SHA-256 al fiecărui fișier, nu conținutul lui.
- Jurnalul local numește fișierele fiecărei scrieri (cel mult 20, apoi „+N”), nu doar numărul lor; niciodată conținutul.
- Documentat: reluarea unei sesiuni readuce copia la ultimul checkpoint și pierde modificările făcute acolo de mână; numele care diferă doar prin normalizarea Unicode nu sunt tratate ca același fișier pe macOS; fereastra rămasă la merge, acum că git nu mai înlocuiește fișiere ignorate.

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

### Corecturi după auditul din 19 noiembrie 2026 (înainte de orice lansare)

Nimic din 1.0.0 nu a fost publicat; corecturile de mai jos fac parte din prima versiune lansată. Fiecare are un test de regresie.

Expunere și execuție
- Git nu mai intră în submodule: `git_diff` nu mai arată conținutul lor (inclusiv fișiere reținute) și nu mai rulează programe din configurația lor.
- `.repochatignore`: un BOM la început nu mai anulează prima regulă; o negație nu mai readuce fișiere dintr-un director exclus; un fișier care nu este text oprește servirea.
- `.repochatignore` este evaluat de git însuși (`git check-ignore`, într-un repository gol și izolat), nu de o reimplementare a regulilor lui: prima variantă a reparației copia comportamentul unei singure versiuni de git și servea fișiere pe care o versiune mai nouă le ignoră. Căile care există doar în istoric sunt judecate la fel, în loturi; fără răspunsul lui git nu se servește nimic.
- Numele fișierelor reținute nu mai pot fi ghicite din numărători care depindeau de interogare.
- Revizii: formele care caută în mesajele commiturilor sunt refuzate; „inexistent” și „neaccesibil” au același răspuns.
- `--no-history` acoperă și `git_diff`; `git_status` și `repo_overview` nu mai arată date din commituri.
- `git log -S` nu mai rulează un `textconv` configurat în repository.
- Fișierele proprii ale repochat sunt mereu reținute; directorul personal ca rădăcină este refuzat și pentru `repochat mcp`.
- Cheia API nu mai rămâne în mediul serverului MCP: serverul se repornește cu un mediu dintr-o listă albă.
- Tokenurile GitHub „fine-grained” și webhook-urile Slack fără schemă sunt mascate; mascarea se face înainte de tăierea liniilor lungi; caracterele „tag” Unicode sunt afișate vizibil.
- Opțiunile de după un cuvânt rătăcit nu mai sunt ignorate în tăcere (`--deny a b --no-history`): comanda se oprește cu eroare.
- Numele argumentelor și calea repository-ului nu mai ajung în terminal cu secvențe de control.

Robustețe
- O linie construită anume nu mai oprește serverul la căutare; nicio gorutină nu mai poate opri procesul.
- Un client care nu citește `stderr` nu mai blochează serverul.
- `repochat call`, autotestul de la pornire și `doctor` nu mai pierd răspunsul unui apel mai lung de 5 secunde (repository mare, cache rece).
- `read_file`, căutarea și conturul respectă termenul apelului.
- Apelurile dintr-un lot JSON-RPC nu mai blochează sesiunea; `initialize` într-un lot este refuzat.
- Ctrl+C oprește `tunnel-client` și când ieșirea e într-un pipe; Ctrl+C la o întrebare o anulează.
- `git_diff` nu mai rescrie `.git/index`.

Corectitudine
- Căutările cu `^` și `$` nu mai omit potriviri; potrivirile din liniile foarte lungi sunt găsite.
- `complete` este fals ori de câte ori ceva a fost tăiat; linia `@meta` intră în bugetul răspunsului.
- `maxLength` numără caractere, ca în schema anunțată; un text obligatoriu format doar din spații este acceptat.
- `status` și `stop` văd orice partajare, inclusiv una pornită din variabile de mediu, și semnalează un `tunnel-client` rămas fără lansator; `doctor` nu mai bifează ce nu a verificat.
- Jurnalul de activitate: argumentele înregistrate au dimensiune limitată, o intrare uriașă nu mai ascunde restul, rotirea concurentă nu mai pierde istoricul.
- Bugetul implicit al unui răspuns scade de la 200 KB la 32 KB (provizoriu, nemăsurat).
- `git_log`, `git_diff` și `git_blame` acceptă numele de fișiere exact cum apar în liste (cu `\`, cu `~` la început, cu spații la capete).
- Mascarea unei chei private nu se mai oprește la marcajele unui conflict de merge; regulile pentru fișiere secrete nu mai sunt ocolite de spații în jurul numelui.
- `file_outline` nu mai raportează „complet” când lista de definiții a fost tăiată.
- Numele membrilor JSON-RPC sunt citite exact (`"ID"` nu este `"id"`).

Lansare
- Suita pentru accesul la repository rulează în CI și cu git 2.25, 2.34 și 2.43, nu doar cu cel mai nou.
- Fluxul de lansare verifică și macOS, cere ca tagul să fie pe `main` și nu mai depinde de atestări, indisponibile pentru un repository privat pe un cont personal.
