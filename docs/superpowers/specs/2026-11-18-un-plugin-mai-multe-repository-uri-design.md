# Un singur plugin ChatGPT, mai multe repository-uri — design

Proiect: [`nalyk/repochat`](https://github.com/nalyk/repochat) · 2026-11-18 · stare: **propus, neaprobat**

Designul a fost dezbătut cu Codex CLI în trei runde până la consens (`docs/design/hub/`). Tot ce e marcat „decizia mea” nu a fost cerut explicit de proprietar.

## De ce

Azi un plugin ChatGPT este legat de un tunel, iar un tunel servește un singur repository. Pentru două proiecte în paralel trebuie două tuneluri și două pluginuri. Proprietarul vrea un singur plugin, oricâte repository-uri pornite în paralel și nimic de adăugat în ChatGPT când pornește un repository nou.

Varianta simplă (un proces care deschide toate repository-urile, cu un argument `repo`) pierde trei lucruri, iar proprietarul cere ca toate trei să fie rezolvate:

- **L1 Izolarea:** orice conversație ar putea citi tot ce e pornit.
- **L2 Repository greșit:** modelul ar putea numi alt repository decât cel dorit.
- **L3 Un repository per proces:** regula pe care se sprijină garanțiile și testele actuale s-ar inversa.

## Ce garantează designul, exact

Formulările sunt cele agreate cu Codex; nu se promite mai mult.

- **L1:** fiecare citire cere o capabilitate validă în acel moment pentru acel repository și pentru profilul lui de expunere. O capabilitate dovedește permisiunea, nu intenția utilizatorului.
- **L2:** după legarea inițială, o conversație nu poate accesa alt repository fără o aprobare locală explicită a proprietarului. Alegerea repository-ului la **primul** apel rămâne a modelului. Garanția depinde de verificarea pe fir de mai jos.
- **L3:** un repository imuabil per proces de servire. Routerul nu face nicio operație pe repository și nu evaluează nicio politică de expunere. Routerul face parte din baza de încredere; aceasta nu este izolare la nivel de sistem de operare.

## Scopuri, non-scopuri, anti-scopuri

**Scopuri**
- Un plugin, un tunel, mai multe repository-uri simultan; lista de instrumente nu depinde de ce e pornit.
- Prima pornire rămâne la fel de simplă ca azi: `repochat`, un câmp ascuns, gata.
- Fiecare terminal își arată doar activitatea repository-ului lui și îl detașează la Ctrl+C.

**Non-scopuri pentru această versiune**
- Instrument de aprobare apelat de model (`open_repo`).
- Index de simboluri, „pachet de context”, hartă de arhitectură (vezi „Ce urmează”).
- Windows.

**Anti-scopuri** (ar fi un eșec chiar dacă funcționează)
- Un mod „fără cod când e un singur repository”.
- Un instrument care listează repository-urile sau codurile.
- Un „repository activ” global.
- Orice afirmație că o capabilitate dovedește intenția utilizatorului.

## Fapte verificate și o necunoscută

- ChatGPT ajunge la server ca `openai-mcp 1.0.0`, pe MCP 2026-07-28, fără stare (măsurat pe sesiunea reală a proprietarului).
- `tunnel-client` v0.0.21: o singură instanță activă per tunel; un singur proces MCP per canal; canalele se fixează la pornire.
- ChatGPT ține în cache lista de instrumente a unui plugin până la „Refresh tools”.
- Documentația OpenAI descrie `params._meta["openai/session"]` ca identificator anonim de conversație trimis la apelurile de instrumente. **Nu este măsurat** că ajunge prin tunel la acest server și nici că alt apelant nu îl poate imita.

## Arhitectura

Patru roluri, toate în același binar:

1. **Hub** — un proces de fundal pornit automat de primul `repochat` și oprit singur la scurt timp după ce se detașează ultimul repository. Deține blocarea per tunel, pornește `tunnel-client`, ține starea care trebuie să supraviețuiască repornirilor (asocieri, revocări, limitatoare) și este singurul proces repochat care atinge cheia API, pe care o transmite doar lui `tunnel-client`.
2. **Router** — procesul MCP pornit de `tunnel-client` (`repochat mcp --router`). Răspunde singur la `server/discover`, `initialize` și `tools/list` și trimite fiecare `tools/call` procesului repository-ului potrivit, printr-un socket Unix. Nu deschide niciun repository, nu rulează git, nu primește cheia și nici descriptori moșteniți care o poartă.
3. **Proces de repository** — serverul MCP de azi, neschimbat ca reguli, pornit de `repochat` în terminalul acelui repository. Un repository per proces, cu propriile opțiuni de expunere.
4. **Terminalul** — un atașament egal cu celelalte: afișează activitatea repository-ului lui, primește cererile de aprobare, detașează la Ctrl+C.

### Instrumentele

Aceleași 13 instrumente, generate din același catalog, fiecare cu un argument în plus, obligatoriu: `repo`. Lista este statică. Trecerea la această versiune cere o singură dată „Refresh tools”; un apelant cu lista veche, fără `repo`, este refuzat cu un mesaj care spune exact asta. Nu se deduce niciodată un repository implicit.

### Capabilitatea (codul de acces)

- 128 de biți din `crypto/rand`, cu eșec la lipsa de entropie; afișată ca `nume-` plus 26 de caractere base32. Numele e doar pentru lizibilitate.
- Legată de identitatea arborelui de lucru **și** de profilul de expunere pentru care a fost emisă. Nu se refolosește pentru un repository care îl înlocuiește pe altul la aceeași cale; lărgirea expunerii e vizibilă proprietarului.
- Stabilă între porniri, ca să meargă în continuare conversațiile vechi și instrucțiunile unui Project din ChatGPT; `repochat --new-code` o rotește.
- Obligatorie la fiecare apel, inclusiv la `repo_overview`. Cod necunoscut, repository oprit și cod revocat dau același răspuns.
- Eliminată înaintea oricărui jurnal, eveniment, urmă sau mesaj de eroare, inclusiv când apare în alt argument; `--audit-args` nu poate trece peste asta.

### Legarea conversației (rezolvarea L2)

Activă implicit când identitatea conversației este prezentă și verificarea pe fir a trecut.

- Identitatea se ia doar din `params._meta["openai/session"]`, niciodată din argumentele instrumentelor. Legările sunt separate pe tunel/profil și pe identitatea de apelant de încredere, acolo unde există una.
- Primul apel reușit al unei conversații o leagă de exact un repository. Orice apel ulterior din acea conversație cu altă capabilitate este refuzat, chiar dacă e validă.
- Operația „leagă, verifică, admite” este una singură, atomică, în hub, înaintea oricărei trimiteri. Dintre două prime apeluri simultane câștigă exact unul.
- Asocierea conversație → repository este separată de valabilitatea codului. Rotirea revocă codul și aprobările, dar păstrează asocierea. Asocierile supraviețuiesc repornirii routerului și a hubului.
- O asociere expirată sau pierdută nu înseamnă „conversație nouă”: cere reautorizare explicită de la proprietar. Când tabela e plină se refuză înscrieri noi; nu se evacuează nimic.
- **Lucru pe două repository-uri într-o conversație:** refuzul poartă o frază de împerechere, iar proprietarul rulează `repochat allow <frază>` în terminalul celui de-al doilea repository. Fraza e aleatoare, de unică folosință, expiră în minute, e verificată contra coliziunilor și e legată imuabil de conversație, de repository-ul țintă și de generațiile curente. Terminalul arată asocierea existentă și adăugarea cerută înainte de aprobare. Modelul nu poate face acest pas.
- **Mod mai strict, opțional:** `--confirm-new-chats` — primul apel al unei conversații nelegate așteaptă aprobarea proprietarului în terminal. Oprit implicit, pentru că adaugă o apăsare de tastă la fiecare chat nou.
- **Fără identitate pe cerere:** refuz, când legarea e cerută; sau servire doar pe bază de capabilitate, cu notă vizibilă în terminal, când proprietarul permite clienți neidentificați.

### Comunicarea router ↔ proces de repository

Un plic mic, versionat, care duce capabilitatea și metadatele cererii separat de argumentele instrumentului. Procesul de repository verifică întâi capabilitatea, apoi validează argumentele rămase cu schemele de azi. Routerul: instrumente dintr-o listă permisă, corelare pe conexiune și generație, respingerea identificatorilor duplicați, propagarea anulării și a termenelor, aruncarea răspunsurilor întârziate, păstrarea `isError`, a erorilor de protocol și a `resultType`.

### Ciclul de viață

- Atașamentele au „chirii” reînnoite; existența unui socket nu dovedește că procesul trăiește.
- Expunerea nu se reface niciodată din înregistrări salvate, doar din atașamente vii.
- Pornirea concurentă și oprirea la ultimul detașament sunt serializate sub blocarea tunelului.
- Versiunile se negociază; un atașament incompatibil e respins cu un mesaj clar, fără să înlocuiască un hub activ.
- `repochat stop --all` este o stare finală; atașamentele nu o pot anula prin recuperare automată.
- **Revocare (detașare sau rotire):** se invalidează generația de autorizare, se închid conexiunile afectate, se anulează lucrul și se aruncă rezultatele din coadă. După confirmare, niciun rezultat din generația veche nu mai e predat transportului. Ce a fost deja predat poate ajunge la OpenAI și nu poate fi rechemat.
- **Limite:** fără pauză globală prin care un apelant greșit i-ar bloca pe cei valizi; admitere globală mărginită, cote per capabilitate, echitate între repository-uri.

## Pasul 0: verificarea pe fir

Înainte de a activa legarea conversației, pe conexiunea reală a proprietarului, înregistrând doar prezența și amprente sărate, niciodată coduri sau texte:

- prezența și stabilitatea lui `openai/session` între apeluri, în două chaturi simultane, la reîncercări, după repornirea routerului, în chaturi dintr-un Project și în chaturi ramificate;
- separarea identificatorilor și proveniența lor, inclusiv cu metadate contradictorii trimise deliberat de alt client permis.

Rezultate posibile:
- **Trece:** legarea se livrează ca mai sus.
- **Prezent, dar proveniența nu poate fi stabilită:** legarea se livrează descrisă ca protecție împotriva comutării accidentale, nu ca izolare față de un apelant ostil.
- **Absent sau instabil:** se livrează doar capabilitățile, iar L2 se documentează ca „redus, nu rezolvat”.

## Modelul de amenințare, pe adversari

| Adversar | Ce îl oprește |
|---|---|
| apelant la distanță fără capabilitate | capabilitatea obligatorie; niciun instrument de listare |
| conținut ostil din repository (inclusiv un cod străin lăsat în text) | legarea conversației: codul altui repository e refuzat în acea conversație |
| router compromis | nimic în acest design; routerul vede coduri și răspunsuri și e parte din baza de încredere |
| proces local al aceluiași utilizator | nimic în plus față de azi; „nu este un sandbox” rămâne valabil |

## Experiența de utilizare

```bash
cd ~/projects/api && repochat     # pornește hubul, servește „api”
cd ~/projects/web && repochat     # se atașează; servește și „web”
```

Fiecare terminal afișează o linie gata de copiat, cu codul repository-ului și o întrebare de exemplu. Utilizatorul alege pluginul din meniul `@` al ChatGPT și lipește linia. Codul poate fi pus și în instrucțiunile unui Project din ChatGPT, cu explicația că astfel îl primește fiecare conversație din acel Project. Clipboardul nu se suprascrie automat, iar codul nu se pune într-un URL.

Profilele (tuneluri separate) rămân pentru separarea între audiențe diferite.

## Criteriul de acceptare al versiunii

Două conversații ChatGPT reale simultane; izolare între repository-uri verificată cu santinele; respingerea codului greșit; rotirea; lista de instrumente neschimbată la atașări; comportamentul la detașare, cădere și repornire; actualizare compatibilă.

Teste cap-coadă cu santinele: repository-uri concurente, identificatori duplicați și refolosiți, anulare, răspunsuri întârziate, înlocuirea socketului, repornirea routerului, revocare, versiuni amestecate.

## Ce urmează, în ordinea agreată

Evaluarea propunerii „RepoChat 2.0” a dus la această ordine; nimic din ea nu intră în versiunea de mai sus.

1. **Măsurare și benchmark.** Contabilizarea octeților, limită explicită `max_bytes` (orice conversie în tokeni e etichetată ca estimare), încercarea unei limite implicite de 16–32 KiB. Benchmark fără model în buclă: trei instantanee înghețate (Go, TypeScript/JavaScript, PHP), opt sarcini fiecare, set de evaluare sigilat, linie de bază deterministă construită din instrumentele de azi, prag stabilit dinainte (de exemplu cu cel puțin 30% mai puțini octeți sau mai puține apeluri la o acoperire comparabilă, fără nicio scurgere de politică).
2. **`repo_map` și `get_context` euristic**, livrate doar dacă trec pragul.
3. **Simboluri în memorie:** `find_symbol`, `symbol_context`, apariții etichetate „lexical, nedovedit”, `analyze_changes`. Persistența pe disc doar dacă un cost măsurat o justifică, și atunci ca optimizare privată, niciodată ca sursă de autorizare.
4. **Hartă de importuri și manifeste**, etichetată pe dovezi.

**Amânate, nu respinse:** serverele de limbaj rulează unelte definite de proiect, deci vin doar în interiorul granițelor de execuție din [`2026-11-25-cli-parity-design.md`](2026-11-25-cli-parity-design.md), întâi ca diagnostice de compilator, apoi navigare semantică pentru un singur ecosistem, când măsurătorile o cer (consens cu Codex, `docs/design/cli-parity/`).

**Respinse:** „Agent Context Bridge” (fișierele din repository se pot citi deja; transcrierile din afara lui ar rupe granița), un instrument separat `impact_analysis`, repoziționarea produsului înainte de măsurători.

## Lăsat constructorului

Formatul exact al plicului IPC, structura directorului de rulare, intervalele de chirie și de expirare, forma stocării asocierilor, textele mesajelor.

## Precizări după verificarea de către un cititor independent

Un inginer care nu a participat la discuție a citit documentul și a pus opt întrebări. Răspunsurile de mai jos sunt **decizia mea**, nu cerințe ale proprietarului; singura cerință a lui este: un singur plugin, mai multe repository-uri în paralel, cu cele trei pierderi rezolvate și cu prima pornire la fel de ușoară.

1. **Pasul 0 se livrează întâi, separat.** O versiune-sondă înregistrează local doar prezența și amprentele sărate ale metadatelor. Proprietarul o rulează în două chaturi; rezultatul se salvează per instalare, în configurație, și decide care dintre cele trei variante se activează. Restul se construiește în spatele acestui comutator. Proba cu „alt client permis” se face cu un al doilea client MCP pus de noi pe același tunel simulat; pe conexiunea reală se verifică doar ce trimite ChatGPT. „Identitate de apelant de încredere” înseamnă, azi, nimic mai mult decât tunelul și profilul.
2. **Asociere pierdută față de conversație nouă.** Hubul ține o „epocă” a stocării. Dacă stocarea lipsește sau e coruptă, epoca se schimbă și toate conversațiile cer aprobare în terminal până când proprietarul confirmă că acceptă starea nouă. Asocierile nefolosite 30 de zile devin pietre de mormânt (doar amprenta), deci tot cer reautorizare. Tabela plină: se refuză înscrieri noi; `repochat chats --prune` o curăță la cererea proprietarului.
3. **Fără `openai/session` pe cerere:** refuz implicit când legarea e activă. `--allow-unidentified` permite servirea doar pe bază de cod.
4. **Cod și profil de expunere.** Profilul înseamnă opțiunile care schimbă ce se vede: `--tracked-only`, `--no-history`, `--no-redact`, `--show-withheld-names`, `--allow`, `--deny`. Îngustarea păstrează codul. Lărgirea emite un cod nou și îl anulează pe cel vechi, cu mesaj în terminal.
5. **Opțiunile de legare sunt ale hubului**, nu ale unui repository. Le stabilește pornirea care creează hubul (sau configurația); un terminal care cere altceva e refuzat cu explicație.
6. **O aprobare `repochat allow`** ține cât conversația, până la rotirea codului oricăruia dintre repository-uri sau până la `repochat revoke`. Se păstrează împreună cu asocierile. Nu există în varianta doar-cu-coduri, unde codurile sunt suficiente.
7. **Căile locale rămân ca azi.** `repochat mcp` pe stdio, `mcp-config`, `call` și `doctor` servesc un singur repository, fără tunel și fără cod. Anti-scopul „fără cod” privește doar tunelul. Routerul adaugă `repo` la același catalog.
8. **Profile și actualizare.** Un profil legat cu `--bind` își păstrează sensul: tunelul lui refuză alt repository. A doua pornire pe același tunel înseamnă atașare, nu preluare. Dacă tunelul e ținut de o versiune veche, cea nouă refuză și cere oprirea ei.
