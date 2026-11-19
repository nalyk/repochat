# Jurnal de modificări

Proiect: [`nalyk/repochat`](https://github.com/nalyk/repochat)

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
- Fluxul de lansare verifică și macOS, cere ca tagul să fie pe `main` și nu mai depinde de atestări, indisponibile pentru un repository privat pe un cont personal.
