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
