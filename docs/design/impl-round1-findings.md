# Revizia implementării, runda 1 (Codex CLI)

Codex a citit codul, a rulat `go vet` și `go test -race` și a construit probe proprii contra binarului. Runda nu s-a încheiat cu un răspuns structurat: după ultima probă, serviciul OpenAI a oprit răspunsul cu un filtru de siguranță („possible cybersecurity risk”), declanșat de conținutul probelor. Constatările de mai jos sunt extrase din jurnalul de execuție al lui Codex (comenzile rulate și ieșirile lor), nu dintr-un verdict final.

Toate au fost reproduse, reparate și acoperite cu teste de regresie (`internal/repo/review_test.go`, `internal/tools/tools_test.go`, `internal/mcp/server_test.go`).

| # | Proba lui Codex | Problema | Reparația |
|---|---|---|---|
| 1 | `META_RAW_TOKEN: True` | linia `@meta` nu era mascată: un nume de fișier care arată a token apărea în clar | valorile din `@meta` trec prin aceeași mascare ca textul |
| 2 | `IGNORE_LEAK: True` | tiparul `data[[:digit:]].txt` din `.repochatignore` nu era înțeles, deci fișierul era servit | clase POSIX suportate; un tipar neînțeles oprește totul în loc să fie sărit |
| 3 | `MAILMAP_OUTSIDE: True` | `mailmap.file` din configurația repository-ului făcea ca `git blame` să afișeze conținut dintr-un fișier extern | `mailmap.file`, `mailmap.blob`, `log.mailmap` neutralizate |
| 4 | citirea și diff-ul unei chei PEM | ultima linie, scurtă, a cheii și linia `END` rămâneau vizibile; contextul din antetul de hunk afișa o linie din cheie | liniile din bloc se maschează indiferent de lungime; antetul de hunk își pierde contextul când fișierul conține chei |
| 5 | `LONG_HEADER` | un marcaj `BEGIN` împins dincolo de limita unei linii foarte lungi scăpa detecției | marcajele sunt căutate și în partea tăiată a liniei |
| 6 | `MIXED_SECRET` | `AWS_sEcReT_ACCESS_KEY=` scăpa prefiltrului sensibil la majuscule | prefiltru insensibil la majuscule |
| 7 | `GREP_ORACLE … next_skip=4` | filtrul pe mesajul de commit lăsa să se vadă, prin paginare, dacă o presupunere se potrivea cu text mascat | filtrul rulează local, pe mesajul mascat; paginarea numără doar commiturile afișabile |
| 8 | `UNREACHABLE_SERVED True` | autorizarea unui commit rămânea în cache după ce ramura era ștearsă | cache-ul expiră în 3 secunde |
| 9 | `DIFF_LATE_FILTER True` | un filtru `clean` configurat după pornire era executat | lista de filtre se recitește înaintea fiecărei operații care citește arborele de lucru |
| 10 | `PROTECTED_EMPTY_FILTER_EXECUTED True` | un filtru cu nume gol (`filter..clean`) nu era neutralizat | numele gol este tratat ca orice alt nume |
| 11 | `BACKPRESSURE_SIGTERM_HUNG True` | un client care nu mai citea răspunsurile bloca serverul, inclusiv la SIGTERM | scrierea se face printr-o coadă separată; bucla de citire nu mai așteaptă clientul |
| 12 | două teste picate în sandbox | un `.git` rătăcit în `/tmp` era luat drept repository | `.git` trebuie să conțină `HEAD` sau să fie un fișier `gitdir:` |
| 13 | citire de cod | capabilitățile clientului erau verificate doar ca prezență | trebuie să fie un obiect JSON |

În timpul reparațiilor a apărut o problemă din aceeași familie, negăsită de Codex: `blame.ignoreRevsFile` din configurația repository-ului face ca git să citeze prima linie a unui fișier extern în mesajul de eroare. Pe git 2.34 setarea nu poate fi anulată din linia de comandă, așa că textul de eroare al lui git nu mai ajunge niciodată într-un rezultat: rămâne în jurnalul local.

Limită declarată (punctul 9): un filtru adăugat în `.git/config` chiar în timpul unei comenzi nu poate fi oprit. Fereastra a scăzut de la câteva secunde la durata unei singure operații.

## Runda 2: verificarea reparațiilor

Într-o sesiune nouă, Codex a verificat reparațiile prin citirea codului (sandbox-ul lui nu a mai permis rularea testelor). A confirmat 10 din 14 și a arătat, cu linii exacte, de ce patru erau incomplete, plus o problemă nouă. Toate cinci au fost reparate și acoperite cu teste (`impl-round2-codex.json`):

| # | Problema | Reparația |
|---|---|---|
| N1 | identificatorul repository-ului din `@meta` venea din numele directorului și ocolea mascarea | un nume de director care arată a secret este înlocuit cu „repository” |
| N2 | `[!a\-z]` era tradus ca interval `a-z`, deci un tipar valid primea alt sens | caracterele escapate dintr-o clasă sunt scrise ca literali |
| N3 | un marcaj `BEGIN` tăiat exact la limita de 64 KiB era ratat | suprapunerea dintre bucăți se păstrează de la prima bucată |
| N4 | cu coada de ieșire plină, bucla de citire rămânea blocată | bucla așteaptă cel mult 5 secunde, apoi încheie sesiunea |
| N5 | filtrul pe autor din `git_log` era un oracol pentru text mascat din numele autorului | filtrul rulează local, pe forma mascată |

## Ce a găsit CI-ul pe macOS

Prima rulare pe macOS a trecut toate testele Go, dar a picat testul de primă pornire: pe un desktop, asistentul întreba vizibil „Open the tunnels page in your browser?” înaintea câmpului ascuns, iar ce se tasta acolo apărea pe ecran. Întrebarea a fost eliminată; testul verifică acum că nimic nu este întrebat la vedere înaintea câmpului ascuns.

## Runda 3

Codex a rulat de data aceasta suita (`go vet`, `go test -race`, inclusiv testele cu `tunnel-client` real) și a confirmat toate cele cinci reparații. A mai găsit o problemă (P1): un apel terminat își elibera locul în limita de admitere înainte ca răspunsul să intre în coada de ieșire, deci un client care nu citește putea acumula răspunsuri fără limită. Locul se eliberează acum abia după ce răspunsul a fost pus în coadă (`impl-round3-codex.json`).

## Runda 4

Codex a confirmat reparația P1 pentru apelurile individuale și a arătat că loturile JSON-RPC (existente doar în revizia 2025-03-26) o ocoleau: apelurile dintr-un lot își eliberau locul înainte ca răspunsul comun să fie pus în coadă. Un lot are acum cel mult 32 de mesaje, iar apelurile lui își păstrează locul până când răspunsul lotului este în coadă (`impl-round4-codex.json`).
