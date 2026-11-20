# Revizia de design cu Codex CLI

Proiect: [`nalyk/repochat`](https://github.com/nalyk/repochat)

repochat 1.0 a fost proiectat într-o dezbatere adversarială între Claude (autor) și Codex CLI (`codex exec`, sandbox doar-citire, cu căutare web), rulată local. Fișierele de aici sunt transcrierea brută: ce i s-a trimis lui Codex și ce a răspuns, în JSON validat pe o schemă. Singura modificare față de ce s-a primit: prefixul căilor de pe calculatorul local a fost scos din linkuri, așa că ele încep de la rădăcina repository-ului.

| Fișier | Conținut |
|---|---|
| `00-design-brief.md` | propunerea inițială: constrângeri, fapte verificate, defectele MVP-ului, designul D1–D16 |
| `round1-prompt.txt`, `round1-codex.json` | runda 1: verdict pe fiecare punct și 7 probleme omise (M1–M7) |
| `round2-prompt.txt`, `round2-codex.json` | runda 2: concesii C1–C16, dezacorduri X1–X8, cerința de onboarding (D17); 5 probleme noi (M8–M12) |
| `round3-prompt.txt`, `round3-codex.json` | runda 3: textul final al contractului; consens de design |
| `impl-round1-prompt.txt`, `impl-round1-findings.md` | revizia implementării: 13 probleme reproduse de Codex, toate reparate |
| `impl-round2-*` … `impl-round5-*` | verificarea reparațiilor |

## Ce a schimbat dezbaterea

Codex a confirmat defectele MVP-ului și a găsit probleme reale în propunerea inițială. Cele mai importante, toate acceptate:

- **M1 (critic):** un diff cu zero căi permise ar fi devenit un diff nerestricționat. Acum diff-urile se produc pe fișier, iar un commit care atinge doar fișiere reținute nu rulează nicio comandă care produce conținut.
- **D4 (critic):** `os.Root` urmează symlink-urile interne. Accesul la fișiere este acum ancorat pe descriptori verificați, componentă cu componentă.
- **D6 (critic):** `protocol.allow=never` poate fi suprascris din repository, iar obiectele lipsă pot declanșa descărcări. S-au adăugat `GIT_NO_LAZY_FETCH`, `GIT_ALLOW_PROTOCOL` gol, `--git-dir`/`--work-tree` explicite și verificarea accesibilității commiturilor.
- **M2:** căutarea peste text nemascat ar fi fost un oracol pentru secrete. Predicatul rulează acum pe textul mascat.
- **D11:** `tunnel-client` respinge apelurile dacă lipsește `notifications/initialized`; lansatorul activează explicit opțiunea corespunzătoare.
- **M9, M10:** primul apel nu dovedește că apelantul e ChatGPT, iar preluarea unui tunel nu are voie să semnalizeze un PID.

Puncte unde Claude nu a cedat și Codex a acceptat compromisul: configurația git a utilizatorului rămâne activă (X1), rezultatele rămân text cu o linie `@meta` în loc de `structuredContent` (X3), repository-ul poate fi schimbat liber între porniri, cu avertizare, iar legarea profilului de un repository este opțională (X6).

Consensul din runda 3 privește designul. Corectitudinea implementării a fost verificată separat: prin teste, prin rularea cu binarul real `tunnel-client` și prin revizia din fișierele `impl-*`, care a găsit probleme pe care designul agreat nu le prevenea.

## Auditul din 19 noiembrie 2026

| Fișier | Conținut |
|---|---|
| `audit/round1-prompt.txt`, `audit/round1-codex.md` | nouă decizii nesigure din audit, puse lui Codex; verdict pe fiecare |
| `audit/round2-prompt.txt`, `audit/round2-codex.md` | concesii și două contrapropuneri; consens pe toate punctele |

Codex a schimbat patru decizii: repornirea serverului MCP eșuează închis, lista albă de mediu folosește nume explicite, sondarea blocării unui tunel are propria blocare de coordonare, iar garda pentru directorul personal se aplică și comenzii `mcp`. A acceptat, în schimb, păstrarea unui `initialize` repetat în afara unui lot.

## Regula K2, inversată (19 noiembrie 2026)

Regula K2 din [`00-design-brief.md`](00-design-brief.md), „Read-only forever” (niciun instrument care scrie), a fost inversată de proprietar pe 19 noiembrie 2026, pentru un mod de scriere pornit doar explicit, cu `--write`. Fără `--write` regula rămâne în vigoare neschimbată: aceleași 13 instrumente doar-citire. Restul lui K2 rămâne valabil și în modul de scriere: nicio comandă arbitrară, iar modelul nu furnizează niciodată argumente brute pentru git. Designul și ce se schimbă în garanții: [`../superpowers/specs/2026-11-19-write-mode-edits-design.md`](../superpowers/specs/2026-11-19-write-mode-edits-design.md).
