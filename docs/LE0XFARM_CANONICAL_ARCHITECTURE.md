# Le0xFarm — Canonical Product & Architecture

**Status:** CANONICAL DRAFT v1.2 — approved product-stage and operating-safety decisions
**Purpose:** единый источник истины по идее продукта, архитектуре, продуктовым жизненным циклам и ещё не закрытым вопросам Le0xFarm.

**Revision note (2026-09-09):** v1.2 сохраняет решения v1.1 и фиксирует отдельно утверждённые product-readiness stages, initial-user scope, safe migration, maintenance, useful-work, hardware re-resolution, Owner authentication, Telegram linking, operational-secrets и Controller-backup decisions. Вопросы, которые этими решениями не закрыты, остаются **OPEN**.

**Revision note (2026-09-10):** v1.2 дополнена approved FROZEN portfolio/mining-accounting lifecycle, forgotten-asset, market/Telegram intelligence, local SecretProvider input и open-source Core/private official Brain boundaries. Exact provider, ledger, entitlement protocol и legal/commercial mechanics остаются **OPEN**.

## 0. Статусы решений

- **FROZEN** — решение принято и не должно молча меняться реализацией.
- **PLANNED** — направление принято, реализация может отсутствовать.
- **OPEN** — вопрос осознанно не закрыт.
- **REJECTED** — вариант отвергнут и не должен возвращаться без отдельного пересмотра.

Если реализация требует нарушить FROZEN-инвариант, работа должна остановиться и конфликт выносится на отдельное архитектурное решение.

---

## 1. Назначение продукта — FROZEN

Le0xFarm — self-hosted система централизованного управления майнинговой фермой.

Пользователь задаёт **желаемое состояние**, а система сама безопасно приводит реальное состояние к нему.

Примеры:
- «Запусти все процессоры на ZEPH».
- «Переключи все RTX 5090 на PBC».
- «Останови все 4070 Super».
- «Покажи, какие машины сейчас не майнят».
- «Для этого 7950X на ZEPH используй 30 потоков, а на XMR — 12».
- «Если возможно, используй одну общую ноду для всей Farm».

Базовая модель:

```text
Desired State
    ↓
Controller
    ↓
Observed State
    ↓
Reconciliation
    ↓
Agent / Noda
    ↓
Actual State
```

START/STOP/RESTART — средство конвергенции, а не источник истины.

---

## 1A. Product readiness stages — FROZEN

Le0xFarm проходит три разные стадии готовности. Их нельзя смешивать или называть одним неоднозначным `MVP`.

### 1. TECHNICAL MVP

Достаточно полный local Controller/Agent mining-management loop для реального тестирования и dogfooding.

Он должен доказывать безопасный путь:

```text
persistent configuration and Desired State
→ fresh Observed State
→ reconciliation
→ Agent runtime
→ workload-specific useful-work evidence
```

TECHNICAL MVP не обязан включать весь финальный Pro intelligence stack, managed-wallet subsystem, advanced remote convenience или зрелый profitability/autoswitch.

### 2. PAVEL DAILY-USE / PRO BETA

Стадия, на которой Pavel может перестать полагаться на прежние ручные mining workflows и использовать Le0xFarm как основную ежедневную систему.

Она должна включать зрелый Controller/Agent operation и главную практическую причину существования Le0xFarm для Pavel:

- Brain discovery новых mining projects;
- анализ repositories, documentation и releases;
- generation проверяемых Package/Profile recipes;
- Noda, когда она нужна workload;
- wallet subsystem, когда новый проект требует создания/управления wallet;
- remote/Telegram convenience;
- useful-work validation;
- diagnostics и понятный recovery path.
- local portfolio/mining accounting и Brain market/project intelligence для historical, dormant, unpaid и locked assets;
- proactive Telegram portfolio/project/market alerts с честными freshness, provenance и uncertainty.

Profitability/autoswitch может развиваться постепенно, но система уже должна быть осмысленно пригодна для discovery и безопасного запуска новых mining projects.

Если PAVEL DAILY-USE / PRO BETA включает contribution-based Pro entitlement, она не может пройти без первого полного **PRO ABUSE / ENTITLEMENT RED-TEAM GATE** из §36B.

Также обязательны portfolio/market и lifecycle-aware mining-accounting acceptance §27I, а official Brain entitlement boundary проверяется с hostile forked/patched public Controller и direct API client по threat model §28A/§36B.

### 3. PUBLIC RELEASE

Качество для внешних пользователей: безопасные onboarding и migration, recovery, user security, supportability, compatibility policy, updates, export/uninstall, privacy и ясные product guarantees.

PUBLIC RELEASE не определяется только наличием mining loop: весь пользовательский lifecycle должен быть безопасен без участия авторов проекта.

PUBLIC PRO release требует повторного прохождения **PRO ABUSE / ENTITLEMENT RED-TEAM GATE** из §36B против complete production system.

Public release также доказывает public Core/private Brain boundary §28A и provider/portfolio/Telegram quality §27C–§27I/§55F.

---

## 1B. First users and target operator — FROZEN

Первый реальный пользователь Le0xFarm — **Pavel**.

Первичные closed testers могут включать друзей Pavel. Они могут получать **Complimentary Pro** entitlement со всей применимой Pro functionality и **0% developer contribution**.

Первичный target customer profile — владелец/оператор собственной смешанной CPU/GPU mining Farm. Initial product не проектируется прежде всего как ASIC hosting-provider enterprise platform.

---

## 2. Основные компоненты — FROZEN

1. **Le0xAgent**
2. **Le0xController**
3. **Le0xNoda**
4. **Le0xBrain**
5. **Le0xRelay** — дополнительный сетевой компонент

Компоненты могут находиться на одной машине, на разных Host/VM, в одной LAN или через Интернет.

Runtime-коммуникация между компонентами **не строится на SSH**. SSH остаётся только для разработки, ручного администрирования и аварийного обслуживания.

---

## 3. Le0xAgent — FROZEN

Agent устанавливается на управляемом mining-host.

Он:
- собирает hardware inventory;
- передаёт inventory Controller;
- видит текущий mining runtime;
- запускает/останавливает miners по заданию Controller;
- следит за процессами;
- собирает telemetry;
- имеет bounded local watchdog;
- сообщает фактическое состояние Controller;
- хранит локальные verified Packages и нужный cache.

Inventory по возможности включает:
- CPU vendor/model, sockets, cores, threads, NUMA;
- RAM;
- GPU vendor/model, VRAM, PCI identity, GPU UUID;
- driver;
- storage;
- OS/architecture;
- capabilities.

Agent сообщает:
- current Profile/Execution;
- PID;
- miner state;
- hashrate;
- accepted/rejected;
- pool connectivity;
- CPU/GPU usage;
- temperature/power где возможно;
- uptime;
- errors;
- telemetry freshness.

Agent отличает Le0x-owned executions от **UNMANAGED** процессов и никогда молча не принимает/не останавливает unknown process.

При потере Controller уже работающий mining продолжает работать, если это безопасно. Agent не придумывает новый Desired State.

---

## 3A. Migration from an existing Farm — FROZEN FOR INITIAL PRODUCT

Le0xFarm должен уметь безопасно приходить на уже работающую ферму.

До того как Le0xFarm принимает управление resource, Agent выполняет **observe-only scan** существующих/unmanaged mining processes.

Если найден потенциально конфликтующий unmanaged miner, Le0xFarm:

- показывает, что было обнаружено;
- объясняет, что пользователь должен остановить miner вручную;
- блокирует Le0x-managed START на конфликтующем resource;
- не останавливает process;
- не принимает его под ownership;
- не меняет автоматически его configuration.

После того как пользователь остановил прежний miner, Agent повторяет scan. Только если resource свободен и остальные проверки пройдены, Host/resource может стать READY для Le0x management.

Initial migration path:

```text
observe unmanaged reality
→ show conflict and exact resource
→ user manually stops old miner/manager
→ rescan
→ validate resource is clear
→ explicit Le0x Desired State
→ start Le0x-owned workload
→ verify useful mining
```

Automatic adoption/import of already-running processes не является initial migration mechanism. Будущий configuration importer может быть спроектирован отдельно, но не получает ownership живого process и не ослабляет unmanaged safety.

Для первой миграции желательно сохранять понятный recovery/rollback path к предыдущему рабочему способу запуска. Этот безопасный cutover обязателен до ежедневного использования Le0xFarm на существующей Farm.

---

## 4. Profile cache vs authority — FROZEN

Для удобства Agent может локально хранить/кэшировать:
- Profiles, нужные этому Host;
- applied host-specific settings;
- resolved runtime plan;
- Packages.

Но authoritative persistent configuration Farm находится на Controller.

Нельзя допустить split-brain:

```text
Controller: CPUThreads = 30
Agent:      CPUThreads = 12
```

при котором обе стороны считают себя источником истины.

---

## 5. MiningProfile — FROZEN

MiningProfile описывает общую логику «как майнить этот target»:
- Coin;
- Algorithm;
- Adapter;
- Package/Version;
- Pool;
- WalletRef;
- LoginPolicy;
- Mode;
- общие runtime defaults.

Profile не привязан к одному Host и может использоваться многими машинами.

---

## 6. HostProfileSettings — FROZEN PRECEDENCE / PLANNED IMPLEMENTATION

Нужен отдельный слой host-specific tuning.

Примеры:

```text
7950x-1 + ZEPH: CPUThreads = 30
7950x-1 + XMR:  CPUThreads = 12
EPYC-1 + ZEPH:  CPUThreads = 64
```

Итоговый runtime plan:

```text
MiningProfile
defaults
    ↓
HostProfileSettings overrides
    ↓
capability / compatibility / hard safety validation
→ ResolvedExecutionPlan
```

DesiredWorkload initial MVP хранит selection/intent, но не произвольные tuning overrides. Команда вроде «run all CPUs on ZEPH» выбирает, что и где запускать, но не меняет CPUThreads, affinity или tuning.

Если requested setting невозможен или небезопасен, resolution завершается видимым `BLOCKED`, а не silent clamp.

Пример:

```text
requested CPUThreads = 128
supported CPU threads = 32
result = BLOCKED, with requested and supported values
```

Temporary runtime tuning может быть позже спроектирован как отдельная explicit feature. Точное имя/схема объекта `HostProfileSettings` остаётся implementation detail, но его ответственность и precedence уже FROZEN.

---

## 7. Le0xNoda — FROZEN DIRECTION / PLANNED

Noda управляет blockchain nodes и связанным node/wallet runtime.

Может работать:
- на mining-host;
- на Controller;
- на отдельном сервере/VM.

Может устанавливаться вместе с Agent, но быть inactive до необходимости. Поддерживает standalone deployment.

Controller может поручить:
- install/configure/start/stop node;
- контролировать sync;
- RPC;
- peers;
- blockchain height;
- health;
- service endpoint.

---

## 8. Shared-node и per-host-node — FROZEN

Если одна node может обслуживать несколько miners, Controller использует общую node.

Если монета/режим требует отдельную node на каждый Host:

```text
Host 1 + Noda 1 + Wallet 1
Host 2 + Noda 2 + Wallet 2
Host 3 + Noda 3 + Wallet 3
```

Controller сохраняет каждый созданный wallet и его backup.

Node topology определяется service/capability metadata конкретной монеты, а не hardcoded логикой core.

Noda/service runtime должен быть отдельным Service domain, а не искусственно втиснутым в MiningWorkload.

---

## 9. Wallet через Noda — FROZEN

Если Noda создаёт wallet, она может временно получить:
- public address;
- wallet files;
- seed/mnemonic;
- spend/view keys;
- другие recovery credentials.

Noda передаёт необходимые данные Controller по authenticated encrypted transport.

Controller:
1. регистрирует Wallet;
2. сохраняет canonical working wallet;
3. создаёт encrypted recovery backup;
4. проверяет backup;
5. сохраняет public WalletRef;
6. только после успешного recovery workflow считает wallet provisioning завершённым.

Plaintext recovery material на Noda удаляется после успешной передачи, если provider больше не требует его для работы.

---

## 10. Le0xController — FROZEN

Для v1:

```text
ONE FARM = ONE AUTHORITATIVE CONTROLLER
```

No active-active/multi-master.

Controller отвечает за:
- Farm topology;
- Hosts/Agents/Noda;
- persistent configuration;
- Desired State;
- Observed State;
- reconciliation;
- Pools;
- WalletRefs;
- real wallet storage;
- wallet backups;
- Profiles;
- HostProfileSettings;
- Packages;
- DesiredWorkloads;
- Groups;
- local portfolio, mining accounting и historical asset awareness;
- balance/provider freshness и local portfolio export;
- Operational Secrets/SecretRefs и local SecretProvider boundary;
- logs/history/audit;
- CLI;
- Web UI;
- local human-language commands;
- Brain integration;
- Telegram through Brain;
- developer contribution scheduling.

---

## 10A. Owner authentication, authorization and roles — FROZEN DIRECTION / PLANNED

Component authentication (Agent↔Controller) недостаточна для зрелого user-facing продукта.

Initial local Controller first-run:

```text
Create Farm
→ Create Owner
→ username
→ password
→ recovery codes
```

Password хранится только через сильный password KDF, например Argon2id; plaintext или reversible password storage запрещены.

Local Web UI требует:

- HTTPS;
- secure cookies/sessions;
- session expiry;
- visibility активных sessions;
- session revocation.

Local/LAN access требует password. TOTP может быть optional в initial implementation. Remote/Relay access требует strong second factor; должны поддерживаться стандартные offline-capable TOTP applications.

Brain account не требуется для authentication в local Controller.

Все management operations должны в перспективе авторизовываться по:

```text
actor + action + target scope
```

Минимальные логические роли:

- **Owner** — trust, recovery, users, entitlements, high-risk approvals;
- **Administrator** — Farm configuration/deployment в разрешённом scope;
- **Operator** — approved start/stop/switch, maintenance, diagnostics;
- **Viewer** — status и redacted history без mutations;
- **Support technician** — временный ограниченный diagnostic scope без wallet/root authority.

**FROZEN:** payout destination changes, wallet secret operations, trust/key changes и ROOT_REQUIRED authorization должны иметь отдельные permissions от обычного mining management.

Initial implementation может иметь одного Owner, но authorization boundary с самого начала использует `Actor + Action + Target`, чтобы роли добавлялись без redesign.

**PLANNED:** локальная Owner authentication обязательна до PAVEL DAILY-USE / PRO BETA; полноценная delegation/RBAC — до PUBLIC RELEASE.

Strong remote authentication, session expiry/revocation, MFA или эквивалентный дополнительный фактор для чувствительного remote management должны быть спроектированы до public release.

---

## 10B. Bulk operations — FROZEN SAFETY SEMANTICS / PLANNED UX

Команды вида:

> «переключи все 5090 на PBC»

не должны быть непрозрачным массовым действием.

Перед выполнением Controller должен уметь определить и, где уместно, показать:

- exact target set;
- exclusions;
- incompatible Hosts/resources;
- Hosts offline/stale;
- уже находящиеся в нужном состоянии resources;
- ожидаемый downtime/disruption;
- потенциальные blockers.

Нужно различать:

- **one-time bulk selection** — normal bulk command; target set фиксируется в момент операции;
- **continuing policy** — правило продолжает применяться к динамически меняющейся группе.

Пример: если в 09:00 команда «Switch all RTX 5090 to PBC» выбрала A/B/C, её target snapshot остаётся A/B/C. RTX 5090, добавленная позже, автоматически не затрагивается. Правило «все текущие и будущие RTX 5090 всегда майнят PBC» — отдельная future POLICY concept.

Partial completion никогда не отображается просто как `SUCCESS` всей операции.

Нужны per-target results и явные offline/pending/excluded targets, чтобы было понятно, что именно не выполнено.

**OPEN:** semantics cancel для уже частично выполненной bulk operation.

---

## 11. Desired/Observed — FROZEN

Desired State — долговременное намерение, не очередь команд.

Если offline Host получил A→B→C, после reconnect применяется только C.

Observed State — текущая reality и должна иметь bounded freshness.

Если Host OFFLINE / NOT READY / STALE:

```text
NO runtime START/STOP
```

до fresh current-epoch observation.

---

## 11A. STOP, confirmation and Maintenance — FROZEN

Нужно строго различать:

```text
STOP REQUESTED
STOP DELIVERED
STOP CONFIRMED
```

Remote Stop request не является доказательством, что unreachable Host действительно остановился.

Requested STOP становится `STOPPED` только после fresh evidence, подтверждающего отсутствие managed execution. Если Agent/Host unreachable, UI не должен говорить пользователю, что mining остановлен.

Нужен отдельный **Maintenance Hold / Maintenance Mode**:

- при входе останавливает managed workloads и, где возможно, подтверждает их остановку;
- ставит выбранный Host/resource под persistent Maintenance Hold;
- блокирует normal Reconciliation от restart mining;
- переживает Controller restart и Agent reconnect без restart mining;
- запрещает Brain/autoswitch/policy возобновлять mining;
- suppresses ожидаемые alerts по явно обслуживаемому объекту;
- не уничтожает Desired configuration: `Desired=RUNNING` остаётся standing intent, подавленным Maintenance Hold;
- требует explicit выхода из maintenance.

EXIT MAINTENANCE снимает hold, получает fresh observation и только после этого возвращает объект в normal reconciliation.

**FROZEN:** software status никогда не должен подаваться как доказательство electrical isolation. Для физического обслуживания пользователь должен понимать, что остановленный miner ≠ физически обесточенное оборудование.

---

## 12. Health polling — FROZEN DIRECTION

Persistent connection + регулярный heartbeat.

Ориентир:
- ~10–30 секунд: heartbeat/liveness;
- ~5 минут: полный health/status refresh.

5 минут — не задержка обнаружения падения.

Controller показывает:
- Host;
- online/offline;
- last seen;
- hardware;
- Profile/Coin/Miner;
- Execution;
- hashrate;
- accepted/rejected;
- pool;
- temperature/power;
- mining uptime;
- errors/warnings.

Future:
- hash/W;
- profitability.

---

## 12A. Useful-work evidence and operational success — FROZEN DIRECTION

Le0xFarm обязан различать уровни доказательства полезной работы.

Это не одно и то же:

```text
process exists
→ hashrate reported
→ resource participation verified
→ pool connected
→ accepted work observed
→ pool accounting observed
→ payout observed/confirmed
```

Разные workload types могут предоставлять разные evidence. AI/compute mining, например, может не иметь традиционного hashrate.

Adapter/Provider сообщает максимум доступных релевантных evidence, например:

- process running;
- runtime healthy;
- job/task received;
- hashrate, если применимо;
- accepted shares, если применимо;
- accepted/completed jobs/tasks, если применимо;
- pool connectivity;
- worker visible on pool, если это проверяемо;
- project/API worker status;
- last successful work;
- reward/account status, если его можно безопасно читать;
- telemetry freshness.

Каждый Adapter/Provider определяет, какие evidence значимы для его workload. `MINING/WORKING` означает: Le0xFarm имеет достаточные свежие evidence именно для этого workload, что полезная работа выполняется. Нельзя придумывать hashrate для workload, у которого его нет.

Нужно явно различать:

- process running;
- useful work;
- accepted work;
- pool-side visibility/accounting;
- payout/reward confirmation.

`MINING` не должно автоматически означать «деньги точно начисляются правильному получателю».

Для каждой важной метрики/факта по возможности нужно знать:

- source;
- freshness/age;
- known / unknown / unavailable;
- confidence или verification level, где уместно.

Unknown нельзя молча показывать как `0` или `healthy`.

Health должен быть layered, как минимум по измерениям:

- Host;
- Agent;
- Execution;
- physical resource participation;
- Pool/useful-work;
- Noda/service;
- payout/accounting где доступно.

---

## 12B. Alerts and incidents — FROZEN DIRECTION / PLANNED

Essential alert evaluation относится к **CORE/FREE** и не зависит от Brain.

Как минимум local Controller должен уметь определить ситуацию «Host/resource expected to work, но не произвёл workload-specific useful work в течение N минут». Brain может добавлять AI interpretation, managed notification routing, richer recommendations и Telegram convenience, но не является prerequisite для basic detection.

Нужно поддержать не просто поток уведомлений, а incident lifecycle:

- severity;
- persistence threshold;
- grouping/deduplication;
- acknowledgement;
- recovery notification;
- maintenance suppression;
- notification delivery test;
- distinction between `acknowledged` и `resolved`.

Один Site/network outage не должен генерировать сотни независимых одинаковых notifications без grouping.

Нужно различать:

- failure;
- missing data;
- intentionally idle/stopped;
- maintenance;
- recovery.

**PLANNED:** Telegram, local UI notifications, email/web push/local notification interfaces.

Proactive Telegram Portfolio / Project / Market Intelligence — это явно **PRO** scope (§27H). Local portfolio/accounting state, provider failures и access к user-owned data остаются CORE/FREE; Brain/Telegram outage не скрывает их.

**PLANNED/OPTIONAL:** внешний dead-man monitor для обнаружения падения самого Controller, поскольку Controller не может надёжно уведомить о собственной полной недоступности.

---

## 13. Groups — FROZEN DIRECTION / PLANNED

SystemGroups автоматически:
- All Hosts;
- All CPUs;
- All GPUs;
- All NVIDIA/AMD;
- Ryzen 7950X/9950X;
- RTX 4070S/4080S/5090;
- Offline/Idle/Mining/Degraded/Error.

UserGroups создаёт пользователь.

Groups используются для targeting/organization.

---

## 14. Human-language commands — FROZEN

Pipeline:

```text
User text
→ Intent parser
→ structured validated intent
→ target/group resolution
→ Desired State mutation
→ normal Reconciler
```

Никакого прямого shell/sudo/kill/arbitrary Agent command.

Free Controller локально понимает ограниченный безопасный набор RU/EN команд.

Если команда не распознана однозначно:

```text
DO NOTHING
```

и ответ:
- «Неизвестная команда»
- “Unknown command”

Без опасного угадывания.

Сложный natural language в Pro может обрабатываться Brain, но Controller заново валидирует structured intent.

---

## 15. Multilingual product — FROZEN

Обязательные языки:
- Russian;
- English.

Другие должны добавляться без изменения business logic.

Internal codes language-neutral:

```text
PROCESS_CRASHED
MINING
RUNNING
STOPPED
```

Presentation использует localization keys/resources.

Локализуются:
- Web UI;
- CLI human output;
- user-facing errors;
- suggested fixes;
- installer/setup;
- Telegram/notifications;
- local parser responses.

Fallback language: English.

---

## 16. Package architecture — FROZEN

Package ≠ MiningProfile.

Package содержит:
- PackageID;
- version;
- exact artifact hash;
- executable path;
- file hashes;
- signature/provenance;
- capabilities;
- adapter/platform metadata.

Agent сам не скачивает miner из Internet.

Flow:

```text
Controller/trusted catalog
→ download
→ verify
→ Controller cache
→ Agent
→ Agent verify
→ local Package Store
```

Exact executable обязательно входит в cryptographic verification.

---

## 17. Miner update / rollback — FROZEN

Хранить минимум:

```text
CURRENT
PREVIOUS
```

Flow:

```text
discover
→ download
→ verify
→ canary
→ rollout
```

При failure:

```text
rollback → PREVIOUS
```

---

## 17A. Update safety contract — FROZEN

`CURRENT/PREVIOUS` — полезная часть update architecture, но не гарантия rollback сама по себе.

Каждый поддерживаемый update должен уметь явно сообщить:

- compatibility requirements;
- expected interruption/restart;
- whether persistent data/schema changes occur;
- whether rollback is technically supported;
- recovery prerequisites;
- whether previous version remains security-approved.

Например, downgrade binary после irreversible node DB migration может быть невозможен.

Нужны/планируются:

- version pinning;
- canary;
- rollout rings;
- health-based promotion;
- maintenance windows;
- known-bad release blocklist;
- artifact/package revocation;
- compatibility matrix.

**FROZEN:** `PREVIOUS` не означает автоматически `SAFE` — предыдущий artifact может быть revoked из-за supply-chain/security incident.

---

## 18. Adapter/provider architecture — FROZEN

Miner/coin-specific logic изолируется через:
- MinerAdapter;
- Package;
- WalletProvider;
- NodeProvider / ServiceAdapter;
- capability metadata.

Нельзя размазывать по core `if coin == ...`, `if miner == ...`, если это можно выразить адаптером/metadata.

---

## 19. Privilege model — FROZEN

Agent не universal root shell.

Default production user: `le0x`.

По умолчанию:
- no password login;
- no interactive shell;
- no SSH login;
- no arbitrary sudo;
- no `NOPASSWD: ALL`.

Отдельный `le0x-helper` работает root и принимает только typed allowlisted operations.

Miner modes:

```text
NONE
PRIVILEGED_PREPARE
ROOT_REQUIRED
```

ROOT_REQUIRED требует explicit approval, привязанного к exact:
- PackageID;
- Version;
- Artifact Hash.

Новая версия/hash требует нового approval.

---

## 20. Wallet architecture — FROZEN

Controller — canonical storage всех Wallets Farm.

Логически:

```text
ControllerData/
    wallets/
    wallets_backup/
```

WalletRef — public mining-facing reference, не secret store.

WalletRef также может быть minimum-authority read-only input для local balance observation. Portfolio identity и accounting определяются §27C–§27F; они не превращают WalletRef в secret или transaction authority.

Working wallet path concept:

```text
wallets/<COIN>/wallet_<WalletID>/
```

Искусственная нумерация 001/002/003 не используется как identity.

---

## 20A. Wallet custody scope, payout correctness and product phasing — FROZEN PHASING / OPEN TRANSACTION SCOPE

Le0xFarm должен явно различать:

1. public payout address;
2. pool/account login;
3. watch-only wallet;
4. hardware wallet reference/integration;
5. locally stored spend-capable working wallet;
6. transaction construction;
7. transaction signing/broadcasting.

Регистрация Wallet или хранение wallet-файла **не означает автоматически право Le0xFarm переводить средства**.

Нужно различать payout address и pool-account login: это не всегда одна сущность.

Payout configuration должна быть network-aware и, где возможно:

- validate address format;
- validate chain/network;
- учитывать memo/tag/payment ID;
- явно сообщать, если validation unavailable;
- показывать recognisable payout fingerprint;
- требовать intentional confirmation при изменении payout destination.

Initial mining-management capability использует существующий **PUBLIC PAYOUT ADDRESS / WalletRef** и не требует от Le0xFarm хранить seed/private keys. Это позволяет развивать Controller/Agent mining loop без ранней custody пользовательских funds.

Managed wallet creation/storage/recovery — отдельный более поздний subsystem:

- wallet creation;
- recovery material handling;
- Wallet Recovery key;
- backup and restore;
- Noda/Web/CLI Wallet Providers;
- working-wallet protection.

Когда этот subsystem реализован, managed wallet публикует `WalletRef` в уже существующий mining/Profile layer; сам mining layer не становится wallet secret store.

Managed wallet subsystem не нужен для начала early Controller/Agent mining tests и TECHNICAL MVP. Он нужен до полного PAVEL DAILY-USE / PRO BETA там, где discovery и запуск нового project требуют создания wallet.

**OPEN:** будет ли Le0xFarm когда-либо конструировать/sign/broadcast transactions. Это не следует автоматически из текущей wallet architecture.

Текущие Portfolio/Market Intelligence requirements не авторизуют automatic selling, orders, withdrawals, transfers, swaps, migrations или transaction signing (§27H). Future Trading / Assisted Selling / Automated Exit требует отдельного explicit architecture/security decision.

---

## 21. Wallet Recovery backup — FROZEN

Каждый созданный wallet получает encrypted recovery backup, если recovery material существует.

Backup может содержать:
- seed/mnemonic;
- private recovery keys;
- wallet metadata;
- native wallet backup.

Существующий backup никогда молча не перезаписывается.

**OPEN:** immutable/versioned naming. Базовое `<COIN>-<WalletID>.tar.age` недостаточно для repeated backups/re-encryption. Нужен BackupID/generation/version/timestamp + WalletID + RecoveryKeyID metadata.

---

## 22. Wallet Recovery keypair — FROZEN

Один Farm-level Wallet Recovery keypair:
- Public Key;
- Private Key.

Public Key:
- хранится на Controller;
- используется для encryption;
- не secret.

Private Key:
- показывается/экспортируется владельцу;
- после explicit confirmation удаляется из Le0xFarm;
- не хранится в farm.db;
- не отправляется Brain/Relay.

После этого Controller может encrypt, но не decrypt без Private Key пользователя.

---

## 23. Wallet Recovery key replacement — FROZEN

Le0xFarm никогда не заменяет keypair автоматически без explicit user approval.

Если Public Key потерян, Controller предлагает создать новую пару.

При согласии:
- новый Public Key становится active;
- старый active Public Key заменяется;
- old backups остаются привязаны к старому RecoveryKeyID;
- пользователь получает явное предупреждение, что старые backups требуют старого Private Key.

Re-encryption старых backups — отдельная explicit операция.

---

## 23A. Wallet backup verification states — FROZEN

Нельзя всё называть одним статусом `Backup verified`.

Нужно различать как минимум:

- **BACKUP_CREATED** — encrypted bundle создан;
- **COPY_INTEGRITY_CHECKED** — stored encrypted bytes/metadata проверены;
- **OFF_MACHINE_COPY_VERIFIED** — подтверждена secondary/offline copy;
- **RECOVERY_TESTED_BY_OWNER** — пользователь с Private Recovery Key реально выполнил restore/decrypt test;
- recovery test timestamp;
- RecoveryKeyID.

После удаления Wallet Recovery Private Key Controller не способен самостоятельно доказать decrypt/restore нового backup — он может проверять только то, что доступно по public-key-only модели и integrity metadata.

Это должно честно отражаться в UI.

---

## 24. Wallet provisioning state machine — OPEN

До реализации Wallet subsystem нужен idempotent workflow.

Не считать wallet READY, пока не существует проверяемая recoverable copy.

Нужно определить поведение при:
- no Recovery Public Key;
- user declines key setup;
- disk full;
- Noda disconnect mid-transfer;
- backup success + DB failure;
- DB success + backup failure;
- crash/retry.

---

## 25. Working wallet protection — OPEN / SECURITY

Нужно решить до Wallet implementation:
- encryption at rest;
- unlock lifecycle;
- process isolation;
- permissions;
- co-located Agent/miner access;
- temporary plaintext;
- reboot recovery.

Encrypted backup не защищает уже unlocked working wallet.

---

## 26. Web Wallet support — FROZEN DIRECTION / PLANNED

Поддерживаем Provider types:
- CLI Wallet;
- Node Wallet;
- RPC Wallet;
- Web Wallet;
- Manual Wallet.

Web-wallet:
- approved URL;
- local user-controlled browser/session;
- CAPTCHA/2FA может требовать человека;
- seed/mnemonic остаётся внутри Farm;
- Brain видит только recipe, не secret;
- unsafe automation заменяется assisted/manual flow.

---

## 27. Offline wallet backup — PLANNED

Encrypted backups копируются на отдельную offline VM/storage.

Private Recovery Key там не обязателен.

---

## 27A. Operational secrets — FROZEN DIRECTION / OPEN STORAGE

Кроме Wallet Recovery существуют обычные operational secrets:

- Pool API credentials;
- private pool-account credentials;
- node RPC passwords/tokens;
- SMTP credentials;
- Telegram Bot Token и required Telegram identifiers;
- Discord bot credentials там, где supported;
- explorer/API credentials;
- read-only exchange API credentials;
- market-data API credentials;
- webhook/notification provider credentials;
- Relay/service credentials;
- future external API tokens.

Они не должны храниться как `WalletRef` или ordinary Profile plaintext.

Profiles и другие ordinary domain objects ссылаются на operational secret через `SecretRef`, а не встраивают plaintext value.

Нужен отдельный secret lifecycle:

- creation/import;
- scope;
- least-privilege use;
- redaction;
- rotation;
- expiry/revocation;
- support-bundle exclusion;
- deletion.

Operational secrets образуют отдельный security domain и не эквивалентны:

- WalletRef;
- wallet recovery material;
- Wallet Recovery key;
- PKI keys.

**OPEN:** exact local encrypted secret store/unlock model.

**FROZEN:** secrets не уходят в Brain без отдельной строго определённой причины и policy.

---

## 27B. Local operational secret input and `.env` — FROZEN PRODUCT / SECURITY DIRECTION

Ordinary integration objects ссылаются на operational secrets только через `SecretRef`:

```text
ExchangeProvider → APIKeySecretRef
TelegramIntegration → BotTokenSecretRef
ExplorerProvider → APIKeySecretRef
```

Le0xFarm может поддерживать local `.env`-style mechanism как convenient self-hosted/development/bootstrap **INPUT** для personal operational secrets, например:

```text
LE0X_TELEGRAM_BOT_TOKEN=...
LE0X_TELEGRAM_CHAT_ID=...
LE0X_SAFETRADE_API_KEY=...
LE0X_COINGECKO_API_KEY=...
LE0X_EXPLORER_API_KEY=...
```

`.env` не является canonical domain model. Это лишь один local `SecretProvider`/input backend; future secure/encrypted providers могут его заменить или дополнить. Exact secret-store encryption, unlock model, parser/library и provider precedence остаются OPEN.

Реальный `.env` с personal data/secrets:

- MUST NOT попадать в Git;
- должен быть excluded через `.gitignore`;
- должен иметь restrictive owner-only permissions, conceptually `0600`, где это поддерживается;
- не должен попадать в normal logs, telemetry, diagnostics/crash reports, Brain requests без explicit need, support bundles или default config export.

Repository может содержать только `.env.example` с variable names, descriptions и fake/example placeholders, никогда с real values. Secret values redacted и, где возможно, не показываются повторно в UI после initial entry.

General operational `.env` **запрещён** как canonical storage для:

- wallet seed/mnemonic/recovery phrase;
- private spend key и wallet private key;
- Wallet Recovery Private Key;
- Farm Recovery private material;
- Controller CA private key;
- Controller identity private key;
- other cryptographic root/recovery keys.

Wallet security, Recovery, PKI и Operational Secrets остаются separate security domains.

Portfolio/market integrations по умолчанию используют **READ-ONLY** exchange credentials. Если exchange имеет scopes, такой credential не имеет trade, withdrawal, transfer или subaccount-management permission. Portfolio observation не получает transaction authority молча.

---

## 27C. Asset identity and CORE portfolio — FROZEN PRODUCT REQUIREMENT

Le0xFarm ведёт local portfolio view всех known mining assets и known WalletRefs/managed wallets, где balance observation технически возможно.

Asset identity не может определяться ticker-only. Canonical identity должна различать Project/Coin, network/chain, contract где applicable, forks, mainnet/testnet и migration identity. Assets с одинаковым ticker, но разными network/chain/contract, никогда не сливаются молча.

Для каждого asset показываются, где доступно:

- Project/Coin и ticker/symbol;
- canonical Network/chain/contract identity;
- WalletRef(s), balance per WalletRef и aggregate balance;
- confirmed, pending/immature и locked balances по их точным semantics;
- balance source/provider, provenance, observed-at timestamp, freshness и provider health/error;
- current market quote, quote currency/source/freshness;
- estimated fiat/stablecoin value;
- confidence/uncertainty, достаточная для honest presentation.

Mandatory distinct balance states:

```text
ZERO BALANCE
BALANCE UNKNOWN
PROVIDER UNAVAILABLE
BALANCE STALE
```

Unknown — это не zero. Provider failure не может производить fabricated zero.

Balance observation использует minimum authority. Где возможно, public payout address/WalletRef достаточен. Read-only sources могут включать local wallet RPC, Noda/node RPC, official project API, official или approved third-party explorer/API, read-only exchange API и Manual Provider. Balance display не требует seed, private spend key, recovery phrase, transaction-signing authority или exchange withdrawal authority. Brain никогда не требует wallet secrets для portfolio calculation.

**CORE/FREE** включает:

- known WalletRefs и locally obtainable balances;
- portfolio inventory и historical/mined asset inventory;
- balance freshness и provider status;
- local portfolio UI/API/CLI по мере реализации;
- local export user-owned portfolio information.

Brain outage, Pro suspension/expiry и developer-service failure не скрывают WalletRefs, known balances, portfolio history и locally obtainable portfolio data. User-owned portfolio information никогда не становится hostage to Pro.

---

## 27D. Mining accounting ledger and reward lifecycle — FROZEN PRODUCT REQUIREMENT

Portfolio работает как mining accounting, а не как набор independent balance snapshots. Le0xFarm понимает lifecycle одних и тех же economically attributable rewards:

```text
MINED / EARNED
→ IMMATURE / LOCKED
→ MATURED
→ POOL AVAILABLE / UNPAID
→ PAYOUT PENDING
→ PAYOUT SENT
→ WALLET RECEIVED
```

Где provider semantics это позволяют, Le0xFarm различает:

- IMMATURE mining rewards;
- LOCKED mining rewards;
- PENDING payout;
- UNPAID / pool balance;
- AVAILABLE pool balance;
- PAID / historical payouts;
- WALLET confirmed balance.

Эти states не склеиваются в одно число. Transition between stages не увеличивает total attributable amount, если не были реально earned new rewards.

```text
Before maturity:
Immature: 300 PBC
Pool available: 0 PBC
Wallet: 100 PBC
Total attributable: 400 PBC

After 100 PBC matures:
Immature: 200 PBC
Pool available: 100 PBC
Wallet: 100 PBC
Total attributable: still 400 PBC
```

### No double counting — FROZEN

Accounting model предотвращает, где evidence это позволяет:

- repeated counting immature reward после maturity;
- repeated counting pool balance после payout;
- repeated counting payout после wallet receipt;
- duplicate discovery одного payout через Pool и Wallet providers;
- duplication после Controller restart, provider retry или repeated API history;
- attribution одной physical reward к several lifecycle stages.

Где exact correlation невозможна, Le0xFarm показывает uncertainty, а не exact-looking aggregate. Accounting correctness важнее convenient total.

Evidence может приходить из Pool API, miner/provider API, chain/node, wallet, explorer, payout transaction, project-specific reward API и Manual accounting source. Observation/event сохраняет provenance, достаточную для объяснения number. Exact ledger/event schema, correlation identifiers и reconciliation algorithm остаются OPEN.

Accounting должен уметь показать today, yesterday, last 24 hours, week, month и custom range, где evidence sufficient:

- newly earned units by Project/Coin;
- attribution by Host/resource где доказуемо;
- matured, paid и received by known WalletRefs;
- current immature/locked, pool unpaid и wallet amounts.

`Newly mined` не выводится просто из wallet balance delta, если transfers/payouts могут исказить result.

Где есть reliable market data, можно хранить estimated value when earned, matured/paid и current value с quote source, timestamp, freshness и uncertainty. Если historical price не был надёжно известен, он не фабрикуется.

Presentation явно различает:

1. newly earned mining production;
2. total economically attributable assets;
3. spendable/available assets;
4. immature/locked assets;
5. unpaid/pending pool assets;
6. wallet balances;
7. historical paid amounts.

`PAID HISTORICALLY` не добавляется автоматически к `CURRENT PORTFOLIO`: funds могли быть transferred, sold, swapped или иначе выведены из tracked WalletRefs. Reduced wallet balance не равен loss/corruption. Transaction-aware outflow/sale accounting — future separate design; automatic trading остаётся OUT OF SCOPE.

Accounting status не скрывает gaps за exact total. Conceptually нужны `ACCOUNTING COMPLETE`, `ACCOUNTING PARTIAL`, `ACCOUNTING UNKNOWN`, `PROVIDER UNAVAILABLE`, `RECONCILIATION REQUIRED`; exact naming остаётся OPEN.

Daily/weekly/monthly local Mining Accounting summary относится к CORE/FREE. Brain interpretation и proactive Telegram delivery таких summaries — PRO.

---

## 27E. Reward maturity, refresh and lifecycle persistence — FROZEN PRODUCT REQUIREMENT

Где Pool/Project/chain публикует maturity/unlock evidence, Le0xFarm хранит:

- unlock/maturity timestamp;
- block/maturity height;
- unlock epoch или vesting schedule;
- remaining blocks/time;
- staged unlock amounts.

Если rewards unlock in stages, stages сохраняются отдельно. Exact amount/time не изобретается. Если provider знает только `IMMATURE`, presentation говорит `UNLOCK DATE: UNKNOWN`, а не гадает.

Le0xFarm периодически refresh observable wallet balances, pool unpaid/available balances, immature/locked rewards, pending payouts, paid history и maturity/unlock state. Exact intervals configurable/OPEN; architecture поддерживает:

- provider-specific cadence;
- более частый refresh active mining/pool accounting;
- более редкий refresh dormant wallets;
- event-driven refresh, где available;
- forced refresh после payout, unlock, listing, migration и provider reconnect;
- rate limits, retry/backoff и API quotas.

Третьи стороны не опрашиваются непрерывно и безотносительно к quotas.

Portfolio totals conceptually distinguish:

```text
SPENDABLE NOW
+ PENDING / UNPAID
+ IMMATURE / LOCKED
= TOTAL ECONOMICALLY ATTRIBUTABLE ASSETS
```

Locked/immature не показываются как spendable/sellable. Если есть market quote, estimated values показываются separately для available, locked/immature и total attributable с explicit warning.

Conceptual maturity/accounting events:

- `REWARD_MATURING_SOON`;
- `REWARD_UNLOCKED`;
- `PAYOUT_READY`;
- `PAYOUT_SENT`;
- `PAYOUT_RECEIVED`.

Exact event schema OPEN. Meaningful events могут вызывать PRO Telegram notification после fresh balance/market refresh, где practical. Notifications deduplicated.

Stopping mining или deleting/deactivating DesiredWorkload, MiningProfile или Package не удаляет known earned, unpaid, immature/locked rewards, payouts и asset awareness. Tracking продолжается, пока они mature, paid, reach wallet, demonstrably expire/become invalid или user explicitly removes tracking. Accounting history — persistent Farm state, не transient telemetry.

---

## 27F. Historical and forgotten assets — FROZEN PRODUCT REQUIREMENT

Le0xFarm сохраняет awareness об assets, исторически mined Farm, даже после:

- DesiredWorkload stopped;
- MiningProfile inactive/deleted;
- miner/Package removed;
- long inactivity.

Stopped mining не удаляет portfolio relevance. Product может rediscover dormant asset, когда появляется verified market, delisting, swap/migration, wallet upgrade или EOL notice, особенно при known non-zero/unpaid/locked balance. Exact retention policy остаётся OPEN.

---

## 27G. Market events and valuation correctness — FROZEN PRODUCT / SAFETY REQUIREMENT

Product-level `MarketEvent` concept минимум различает:

- `LISTING_ANNOUNCED`;
- `DEPOSITS_OPEN`;
- `TRADING_OPEN`;
- `WITHDRAWALS_OPEN`;
- `DELISTING_ANNOUNCED`;
- `DELISTING_IMMINENT`;
- `DELISTED`;
- `SWAP_REQUIRED`;
- `MIGRATION_REQUIRED`;
- `NETWORK_MIGRATION`;
- `MAINNET_LAUNCHED` where relevant;
- `WALLET_UPGRADE_REQUIRED`;
- `PROJECT_EOL` / `SHUTDOWN` where relevant;
- `IMPORTANT_PROJECT_NOTICE`.

Exact schema OPEN. Critical distinction:

```text
listing announced
→ deposits may open
→ trading actually opens
→ withdrawals may open
```

`LISTING_ANNOUNCED != TRADING_OPEN`. Social/announcement evidence о future listing не превращается в fabricated live market.

Portfolio valuation — **ESTIMATE**, а не guaranteed realizable proceeds. Где available, evidence включает exchange, pair, current/last/mark price, quote timestamp, market status, volume/liquidity/spread, deposit/withdrawal status и source confidence.

- insufficient confidence → balance shown, valuation `UNKNOWN` / `UNCERTAIN`;
- stale quote → explicitly stale;
- halted trading → no normal live valuation without warning;
- thin/illiquid market → conservative presentation and material uncertainty disclosure;
- locked rewards → not described as presently sellable.

Future liquidity-adjusted liquidation value, exact quote/price algorithm, provider precedence и confidence scoring остаются OPEN.

---

## 27H. Brain portfolio intelligence and Telegram alerts — FROZEN PRO PRODUCT REQUIREMENT

PRO может давать:

- automatic project monitoring;
- first-listing discovery и verification;
- trading/deposits/withdrawals-open detection;
- delisting/deadline, project EOL, swap/mainnet/network migration и wallet-upgrade intelligence;
- project releases/security notices;
- market-price/status monitoring и cross-source verification;
- dormant/forgotten-asset rediscovery;
- richer valuation and event prioritization by known balance/value;
- proactive Telegram delivery;
- periodic intelligent accounting summaries.

Brain коррелирует:

```text
verified MarketEvent
+ canonical Asset/Network identity
+ Controller-known balance/accounting state
+ balance freshness
+ verified market quote/status
= actionable portfolio intelligence
```

Known non-zero, valuable, unpaid или locked balance повышает priority. Exact prioritization algorithm OPEN. Market listing может быть important, даже если rewards ещё locked; alert отдельно показывает spendable, pool available, pending payout, immature/locked, next evidenced maturity stage и total attributable, не называя locked amount sellable.

Proactive Telegram Portfolio / Project / Market Intelligence — **PRO**. High-value message содержит, где trustworthy data exists:

- event type, Project/Coin и canonical Network;
- exchange/market/pair и actual trading state;
- known spendable, unpaid/pending, immature/locked и total attributable balance;
- current price, quote currency и **approximate** portfolio value;
- balance/quote freshness;
- deposits/withdrawals state;
- deadline/recommended user action;
- source provenance/confidence;
- verified official market/announcement link.

Value language явно говорит `Оценочная стоимость`, `≈`, `по текущей доступной цене`; не обещает продажу всего amount по этой цене. Unknown balance не заменяется zero; unknown/stale quote, unopened trading, thin market и disabled deposits/withdrawals показываются честно.

High-value events включают first verified listing, listing announcement, trading/deposits/withdrawals open, delisting/imminent delisting, swap/migration, wallet upgrade, dormant asset becoming tradable, project shutdown, reward maturing/unlocked и payout transitions. Repeated polling/provider delivery не spam one event: event lifecycle/deduplication — required direction; exact timing/thresholds OPEN.

### Trusted-link and social-source safety — FROZEN

Telegram не становится phishing delivery mechanism. Market/trading URL берётся из или verified against trusted approved official sources. Arbitrary URLs из Discord, Telegram, X/social, community forums и AI-generated text не пересылаются вслепую. Где possible, provenance различает official exchange market page, official exchange announcement и official project announcement. Exact trusted-domain/catalog mechanism OPEN.

Le0xFarm не строится на automation normal Discord user account владельца, не требует его Discord password/session token и не использует self-bot impersonation. Compliant sources могут включать official bot, announcement-channel following, webhook/feed, official Telegram API/channel, GitHub releases, project websites, RSS и official exchange sources. Social post — evidence/provenance, не truth; important events по возможности подтверждаются stronger sources.

Source provenance conceptually distinguishes official exchange/API, official project site/announcement, official repository/release, explorer/node evidence, established market-data provider, secondary aggregator, community/social и unknown/unverified source. Exact confidence formula OPEN. UI объясняет, почему event считается occurred; unexplained AI conclusion не показывается verified fact.

### Privacy boundary — FROZEN

Controller локально знает Asset identity, WalletRefs, balances и accounting. Brain получает only minimum sanitized context — conceptually canonical Asset/Network identity, aggregate balance где necessary, freshness/status — когда это нужно для Pro correlation. Brain не требует raw wallet addresses, если может выполнить role без них, и никогда не получает seeds, private keys, wallet passwords или operational API secrets без separately approved narrow integration.

### Current transaction boundary — FROZEN

Этот product area не авторизует automatic selling, market/limit orders, exchange withdrawals, asset transfers, automatic swaps/migrations и transaction signing. Future Trading / Assisted Selling / Automated Exit требует отдельного explicit architecture/security decision. Read-only monitoring keys не получают trading/withdrawal authority заранее.

---

## 27I. Portfolio/accounting Pro beta acceptance — FROZEN RELEASE GATE

До PAVEL DAILY-USE / PRO BETA нужен synthetic/non-production end-to-end scenario:

1. historical tracked asset с canonical Asset/Network identity существует;
2. Controller знает non-zero balance/accounting state;
3. Brain discovers listing и verifies authoritative evidence;
4. actual trading status и market quote obtained separately;
5. Asset/Network identity matches without ticker collision;
6. Controller передаёт только permitted minimum balance context;
7. estimated value рассчитана с uncertainty;
8. Telegram получает exactly one deduplicated notification;
9. message содержит asset/network, event/trading status, known balance, approximate value, freshness/confidence и verified official link;
10. seed/private key/trading credential не exposed.

Отдельный lifecycle/accounting scenario доказывает:

1. newly mined rewards появляются as immature/locked, затем приходят new rewards;
2. provider даёт available, immature/locked и historical paid amounts;
3. locked rewards содержат multiple evidenced maturity stages;
4. part matures into pool available, payout becomes pending/sent и reaches WalletRef;
5. Controller restarts between stages, including immediately around unlock;
6. mining completely stops, но asset/rewards/history remain tracked;
7. later provider/time state advances и maturity обнаруживается;
8. transitions не меняют total attributable без genuinely new earnings;
9. spendable, unpaid/pending, immature/locked, wallet и historical paid totals update without double counting;
10. repeated/reordered provider events и Pool+Wallet discovery одного payout не duplicate accounting;
11. period earnings remain historically queryable;
12. Telegram unlock notification generated once;
13. later listing alert separately reports spendable, locked/immature, total attributable, estimated value и explicit not-currently-sellable warning.

Fail-safe degraded cases включают:

- balance or maturity date UNKNOWN;
- provider unavailable during expected maturity;
- quote UNKNOWN/stale;
- listing announced while trading not open;
- wrong-network/same-ticker collision;
- suspicious URL;
- exchange API, Brain или Telegram temporarily unavailable;
- unlock amount changes и payout occurs between polls;
- duplicate provider event, stale provider state, provider reset и missing history;
- wallet amount decreases;
- unreconciled/UNKNOWN accounting state.

Все cases показывают uncertainty и fail safely. No asset/reward может быть silently lost, duplicated или counted twice.

---

## 28. Le0xBrain — FROZEN DIRECTION / PLANNED

Brain — developer-hosted intelligence/service layer, но не runtime single point of failure.

При потере Brain:
- current mining continues;
- Controller работает локально;
- Agent продолжает current execution;
- Free остаётся функциональным.

Brain может:
- discover coins;
- analyze GitHub/releases/docs;
- analyze miners/nodes;
- create Package/Profile/Service/Wallet recipes;
- shared verified catalog;
- diagnostics;
- release monitoring;
- project/market event intelligence и cross-source verification;
- dormant/forgotten-asset rediscovery и portfolio correlation по minimum sanitized context;
- proactive portfolio/mining-accounting Telegram intelligence;
- AI troubleshooting;
- Telegram;
- future profitability/autoswitch/self-healing.

---

## 28A. Open Core and private official Brain boundary — FROZEN PRODUCT / SECURITY DIRECTION

Le0xFarm использует **OPEN-CORE** model. Local Farm-management Core остаётся open source, independently buildable и genuinely useful без developer infrastructure. Official Le0xBrain implementation — proprietary developer infrastructure и не входит в public Le0xFarm source distribution.

### Public/open-source Core — FROZEN

Public Core включает:

- Le0xAgent, Le0xController и Le0xNoda;
- self-hosted Le0xRelay;
- CLI и Web/Core UI where applicable;
- local configuration, runtime и mining management;
- local portfolio/accounting Core data;
- Controller-side Brain client boundary;
- public protocol/data contracts required by Controller;
- signature verification и local validation Brain-produced artifacts;
- all local Free functionality;
- Core export, recovery и uninstall capabilities.

Closing Brain source не оправдывает crippled Core или hidden mandatory Brain dependency. Brain outage, discontinuation или Pro suspension не:

- останавливает current local mining;
- скрывает local configuration, WalletRefs и user-owned portfolio/accounting data;
- блокирует local recovery/export;
- делает Brain prerequisite для ordinary Free runtime.

Pro-only Brain intelligence/services могут стать unavailable. Phone-home-or-stop-mining architecture запрещена.

### Private developer infrastructure — FROZEN

Private developer-controlled Brain infrastructure/repository может содержать:

- official Le0xBrain server implementation и AI orchestration;
- project/coin discovery, repository/document/release analysis и market/listing pipelines;
- verified shared-catalog и recipe/Profile/Package generation infrastructure;
- catalog signing infrastructure;
- official entitlement backend и contribution-accounting authority;
- managed Telegram intelligence и other developer services;
- future profitability/autoswitch intelligence pipelines;
- private developer operations.

Public Core repository не публикует Brain server, private AI/crawlers/catalog pipelines, private entitlement backend, private operations или private signing keys только потому, что Controller общается с Brain. Brain internals могут использовать AI services, crawlers, databases, queues, indexes, market feeds, official APIs и protected credentials; они не становятся public Core contract, пока externally visible behavior этого не требует.

### Official Pro is a service, not a local boolean — FROZEN SECURITY PRINCIPLE

Official developer-service entitlement не доказывается locally editable `pro=true`, `contribution_paid=true`, farm.db или local configuration. Farm owner контролирует machines и может иметь root, поэтому local state alone не является authoritative proof official Pro.

Official Brain трактует public Controller как untrusted client на service-security boundary. Authorization conceptually привязывает requests к server-verifiable Controller/Farm identity, entitlement/accounting state where applicable, service/session identity, freshness/replay protection и issued signed credentials/equivalent authority. Exact protocol, receipt/token и contribution-proof mechanism остаются OPEN.

**CLIENT ASSERTION IS NOT AUTHORITATIVE PROOF OF OFFICIAL PRO ENTITLEMENT.** Patched Controller не получает official Pro, просто заявляя `Pro`, paid contribution, zero debt, complimentary status или чужую identity.

### Hostile fork/patched client threat model — FROZEN

Предполагается, что technically capable owner может:

- читать, fork, modify и rebuild public Controller/Agent;
- удалять local contribution scheduling и entitlement checks;
- менять database и arbitrary local Farm/Controller state;
- inspect/replay Brain protocol и imitate normal Controller behavior.

Security не полагается на obscurity, hidden constants, obfuscated checks, secrets в public binaries, client-reported entitlement flags или client-reported contribution totals alone. Public wire schemas, response semantics, artifact/signature formats и API meaning могут быть public; secrecy wire format не security control. Exact Brain API stability/third-party compatibility promise OPEN.

Open-source fork technically может изменять local UI/runtime, убрать local contribution behavior, создать integrations или independent Brain-like backend. Это не даёт access к official Le0xBrain, official Pro, managed intelligence/Telegram, official catalog issuance, private infrastructure/data или developer signing keys. Independent backend — другой service, а не bypass official entitlement.

Official Le0xBrain явно отличается от third-party/self-developed/fork-specific Brain-like backend. Third-party backend не становится trusted или official. Support configurable alternative Brain endpoints и его trust/UI/security design остаются OPEN, а не implied requirement official Controller.

### Source license, official service and brand — FROZEN BOUNDARY / PLANNED LEGAL DIRECTION

`SOURCE LICENSE` регулирует use/modify/distribute rights для open-source Core. `OFFICIAL SERVICE ENTITLEMENT` регулирует access к developer-operated Le0xBrain/official Pro. Это separate concerns: possession/modification Core source не даёт official service entitlement. Текущая Core licensing direction не меняется этим document decision; exact legal license/ToS wording не фиксируется здесь.

Code licensing и right to use official Le0xFarm/Le0xBrain names, logos и `official` designation — separate. Before PUBLIC RELEASE нужен trademark/brand policy для forks, modified builds и misleading affiliation. Exact legal text/process остаётся OPEN/future legal work.

Product trust proposition:

> You own and can inspect the software that controls your Farm. The developer does not need to control your Farm to provide Pro. You can continue using the Free Core without developer infrastructure. Official Pro is paid/intelligence infrastructure delivered by the developer, not a DRM switch that owns your machines.

Architecture предпочитает эту boundary local anti-user DRM.

### Private keys and distributed binaries — FROZEN SECURITY REQUIREMENT

Developer private signing keys никогда не попадают в public Core repo, private Brain repo as plaintext source/config, `.env.example`, docs, CI logs, fixtures и diagnostics. Signing material живёт в dedicated protected key/secret-management domain; Controller содержит только public verification material. Brain source leak не должен означать compromise offline signing root.

Public/distributed Controller, Agent, Noda и Relay binaries не embed Brain private API master credentials, developer private signing keys, entitlement authority secrets, crawler/service credentials, infrastructure root credentials или reusable secrets for impersonating Brain. Всё shipped на owner-controlled Host считается extractable.

Private Brain не означает blind trust. Brain output проходит structured local validation; Brain не может arbitrary shell, bypass Package verification/Helper/local safety, kill unmanaged process, alter wallets, obtain wallet secrets или override USER intent. Privacy §29 полностью применяется к proprietary Brain.

---

## 29. Brain privacy and compromise — FROZEN / CRITICAL

Brain не собирает произвольно локальные данные Farm.

Без специальной причины/consent туда не уходят:
- wallet addresses;
- private pool accounts;
- HostID/hostname/private IP;
- topology;
- filesystem paths;
- seed/mnemonic;
- private wallet keys;
- Wallet Recovery Private Key.

Для portfolio/event correlation Controller по возможности передаёт только canonical Asset/Network identity, necessary aggregate balance и freshness/status. Raw WalletRefs/addresses не передаются, если Brain может выполнить role без них. Operational API secrets, wallet passwords и transaction authority не передаются без separately approved narrow integration.

Проектируем так, будто Brain может быть взломан.

Brain не имеет:
- universal shell;
- universal root;
- direct Agent/Noda control;
- wallet recovery secrets.

---

## 30. Signed Brain artifacts — FROZEN DIRECTION

Brain artifacts подписываются:
- Package manifests;
- catalog entries;
- Developer Profiles;
- service/wallet recipes;
- entitlements.

Signing hierarchy:
- offline root;
- limited online signing key(s).

Private signing material живёт только в protected key-management domain и не попадает в source repositories, distributed binaries, `.env.example`, fixtures, logs или support bundles (§28A). Controller получает только public verification material.

Controller проверяет:
- signature;
- hash;
- KeyID;
- scope;
- version;
- expiry/revocation.

Но valid signature ≠ safe executable.

**OPEN:** independent Controller validation model для Brain-delivered executable artifacts.

---

## 31. Telegram account linking — FROZEN DIRECTION / PLANNED IMPLEMENTATION

```text
Telegram ↔ Brain ↔ Controller
```

Brain передаёт structured intent.

Controller проверяет authentication, nonce, timestamp, replay, action, target, permissions, local safety policy.

Local Controller username/password никогда не отправляется Telegram или Brain.

Account linking использует short-lived one-time binding:

```text
Telegram /link
→ one-time short-lived code
→ user confirms/binds code in local Le0xFarm Web UI
→ TelegramIdentity ↔ LocalUserID
```

Допустим equivalent reverse-code flow с теми же свойствами. Telegram не узнаёт local password.

Low-risk management использует linked identity и normal Controller authorization. Sensitive operations требуют подтверждения в local Web UI и могут быть полностью недоступны через Telegram, включая:

- wallet recovery export;
- Wallet Recovery key rotation;
- ROOT_REQUIRED approval;
- destructive wallet/node deletion;
- trust/security changes.

Proactive portfolio/project/market messages и accounting summaries — PRO scope и подчиняются uncertainty, deduplication, provenance, privacy и trusted-link rules §27H. Telegram не получает wallet secrets, trading credentials или transaction authority.

---

## 32. Relay — PLANNED

```text
Agent → Relay ← Controller
```

Relay — dumb/untrusted transport.

Application trust остаётся end-to-end Agent↔Controller.

Self-hosted Relay — Free/open source.
Managed Relay — может быть Pro.

---

## 33. Free — FROZEN

Free — реально полезный local Farm manager.

Free работает как public open-source Core §28A и не имеет hidden mandatory official Brain dependency.

Включает:
- Controller;
- Agent;
- Noda;
- Profiles/Pools/WalletRefs/Packages;
- Desired/Observed/Reconciliation;
- start/stop/switch;
- watchdog;
- telemetry;
- CLI/Web;
- local RU/EN parser;
- SystemGroups;
- self-hosted Relay;
- manual/custom Profile setup.
- known WalletRefs, locally obtainable balances и provider status;
- local portfolio/mining accounting и historical mined-asset awareness;
- local portfolio UI/API/CLI и export user-owned portfolio data по мере implementation.

Нет artificial Host/GPU limits.

Developer contribution: **0%**.

---

## 34. Pro — FROZEN DIRECTION

Pro = тот же runtime core + Brain intelligence/services.

Official Pro service entitlement отделён от Core source license и проверяется official Brain на untrusted-client boundary §28A; local boolean не является authoritative proof.

Может включать:
- AI Discovery;
- shared verified catalog;
- advanced natural language;
- Telegram;
- auto Profile/Package/Wallet recipes;
- diagnostics;
- release monitoring;
- automatic listing/delisting/swap/migration/project intelligence;
- cross-source verification, advanced market-price discovery и dormant-asset rediscovery;
- intelligent event + local portfolio/accounting correlation;
- proactive Telegram portfolio alerts и accounting summaries с verified links;
- remote convenience;
- profitability/autoswitch;
- schedules/policies;
- analytics;
- AI self-healing.

---

## 35. Developer contribution — FROZEN

Normal Pro: **3%**.

Free/developer/complimentary: **0%**.

Учёт per physical mining resource:
- CPU;
- каждая GPU.

Считается только verified USER mining.

Не считается:
- offline;
- idle;
- download;
- startup;
- zero hash;
- node sync;
- developer-side Brain outage.

Для точных 3% total:

```text
developer_time / user_time = 3 / 97
```

24h USER mining ≈ 44m32s developer mining.

User action всегда выше dev mining.

Official contribution-based Pro опирается на official Brain service entitlement boundary §28A. Удаление contribution behavior из local fork может изменить только этот fork; оно не даёт unauthorized official Brain/Pro access. Normal 3%, Developer/Complimentary 0% и applicable trial/test direction не меняются. Exact proof, receipt, debt, scheduling, offline и suspension mechanics остаются OPEN.

---

## 36. Contribution arbitration — OPEN

Нельзя моделировать dev mining как обычный competing DesiredWorkload.

Нужен отдельный arbitration/effective execution layer, который:
- не переписывает user Desired State;
- не заставляет Reconciler бороться с dev workload;
- немедленно уступает новой user action.

Нужно окончательно определить:
- debt;
- interruption;
- expiry;
- repeated preemption;
- suspension;
- Brain outage exemptions;
- debt clearing;
- entitlement behavior.

Ранее рассматривалась модель >72h eligible user mining inability to clear debt → Pro suspension until debt cleared; это нужно повторно подтвердить перед implementation.

---

## 36A. Developer contribution UX and accounting transparency — FROZEN DIRECTION / OPEN DETAILS

3% contribution должна быть видима и объяснима пользователю.

Для каждого physical resource UI/history должны в будущем показывать:

- verified USER mining time;
- contribution accrued/due;
- contribution completed;
- current contribution status;
- excluded intervals and reasons;
- interrupted developer sessions;
- remaining balance/debt;
- applicable entitlement/grace state;
- exportable accounting history.

Нужно явно сообщать, что Le0xFarm 3%:

- не обязательно 3% revenue/profit/electricity;
- не включает автоматически fees/donations стороннего miner/pool;
- измеряется по принятой resource-time accounting model.

Developer execution не получает silently больше:

- CPU threads;
- GPUs;
- power;
- privileges;

чем разрешённый contribution resource envelope.

**OPEN:** maximum uninterrupted developer session duration.

**OPEN:** точное поведение active developer session при Controller outage — оно должно быть bounded и заранее авторизовано, а не продолжаться бесконечно.

**OPEN:** mixed/dual mining accounting и physical-device replacement semantics.

---

## 36B. PRO ABUSE / ENTITLEMENT RED-TEAM GATE — FROZEN / REQUIRED RELEASE GATE

Contribution/entitlement correctness требует обязательной независимой adversarial verification до contribution-based Pro release. Gate симметрично защищает и договорную модель Pro, и пользователя.

Независимый аудит должен доказать обе стороны:

1. Технически способный hostile Farm owner не может получить или сохранять paid Pro functionality, обходя contractually required developer contribution.
2. Le0xFarm никогда не начисляет, не требует и не учитывает **больше** developer contribution, чем точная договорная величина.

Accounting fairness является симметричной. Любой подтверждённый случай:

- overcharge;
- double counting;
- false debt;
- failure to credit valid completed contribution;
- erroneous Pro suspension;
- erroneous interference with USER mining;

является **RELEASE BLOCKER** той же severity, что и успешный fee bypass.

### Adversary model

Независимый auditor обязан предполагать, что пользователь:

- владеет Farm;
- имеет root/administrator access к своим машинам;
- может изучать local files и databases;
- может snapshot/restore VM;
- может клонировать disks и Controllers;
- может менять system time;
- может disconnect/reconnect Brain;
- может убивать local processes;
- может исследовать network behavior;
- может собирать или патчить open-source local components;
- активно пытается обойти contribution accounting.

Нельзя утверждать, что abuse при local root access можно сделать криптографически невозможным. Design должен явно различать:

- Free local ownership и runtime independence;
- local contribution evidence;
- Brain-side entitlement/accounting authority там, где она требуется выбранной моделью;
- signed, replay-resistant receipts или эквивалентный механизм;
- user-owned wallets/config/runtime, которые никогда не удерживаются как заложники Pro.

### Mandatory attack classes

Red-team audit обязан явно искать способы:

- использовать Pro без обязательной 3% developer contribution;
- стереть или уменьшить accrued contribution восстановлением старого Controller backup;
- стереть или уменьшить debt восстановлением VM snapshot;
- клонировать Controller и дублировать или сбрасывать entitlement/accounting state;
- клонировать complimentary/test entitlement на другую Farm;
- replay старого valid contribution receipt;
- replay stale Brain response;
- манипулировать ControllerID / FarmID / HostID / DeviceID;
- заменить hardware после debt или completed contribution;
- манипулировать wall clock, timezone и monotonic-time assumptions;
- отключить Brain точно во время developer mining;
- оставаться на Pro бесконечно при недоступном Brain;
- убивать developer miner сразу после запуска;
- заставить developer execution выглядеть `RUNNING`, не выполняя useful work;
- falsify Agent telemetry;
- falsify pool-side/project-side evidence, где это возможно;
- использовать ambiguous/unknown telemetry как billable contribution;
- одновременно выполнять USER и developer mining на одном physical resource;
- использовать dual/mining или multi-output workload для искажения resource accounting;
- постоянно preempt developer sessions USER actions, избегая contribution;
- использовать Controller crash/restart во время contribution;
- использовать Agent crash/restart во время contribution;
- downgrade до старой Le0xFarm version с более слабым enforcement;
- patch/recompile local Controller или Agent;
- compile Controller с contribution disabled или entitlement always true;
- modify Agent для fabricated developer mining/useful-work evidence;
- удалить local debt или forge contribution evidence;
- вызывать Brain APIs напрямую, минуя normal Controller flow;
- implement custom fake Controller client или bypass official Controller entirely;
- patch local signature/validation result;
- replay old official Brain authorization;
- impersonate another Controller/Farm;
- копировать entitlement material между Controllers/Farms;
- дублировать contribution credit между hardware/resources;
- учитывать одну physical GPU/CPU несколько раз;
- заставить систему забыть already completed contribution;
- заставить contribution превысить точные договорные 3%.

Auditor обязан дополнительно искать attack classes, отсутствующие в этом списке.

Required service-security outcome: hostile locally patched Core может контролировать своё local behavior, но не может обманом получить unauthorized official Brain/Pro service. Public Brain protocol и patched client считаются part of threat model (§28A). Symmetric user-protection requirement ниже остаётся равнозначным.

### User-protection adversarial tests

Аудит обязан активно пытаться заставить Le0xFarm действовать несправедливо **против пользователя**, включая попытки:

- начислить больше 3%;
- неправильно засчитать startup/warmup/failure time;
- засчитать developer mining без useful work;
- дважды засчитать один physical resource;
- удвоить debt после Controller restore;
- потерять completed contribution после backup restore;
- ошибочно перенести старый debt на replacement hardware;
- продолжить developer mining дольше авторизованного интервала;
- продолжить developer mining после потери Controller дольше определённого bound;
- использовать больше CPU threads/devices/power, чем разрешено resource envelope;
- ошибочно блокировать USER START/STOP;
- suspend Pro из-за недоступности самого Brain;
- блокировать доступ к user configuration, wallets, backups, exports или core runtime.

Это не просто UX bugs. Это contribution-accounting correctness/security failures и release blockers.

### Required timing and repetition

Перед PAVEL DAILY-USE / PRO BETA обязателен порядок:

```text
contribution/entitlement design
→ implementation
→ ordinary Sol tests
→ deterministic/adversarial tests
→ independent Astra High red-team audit
→ fixes
→ repeat independent adversarial verification
→ only then Pro Beta gate may pass
```

Перед PUBLIC PRO release независимый red-team audit повторяется против complete production system.

При любом последующем material change contribution/entitlement logic соответствующая adversarial gate должна быть повторена.

Этот раздел **не фиксирует** unfinished contribution accounting algorithm. Debt, interruption, expiry, scheduling, Controller outage behavior, evidence uncertainty, physical resource identity и suspension mechanics остаются OPEN до отдельных решений в §36, §36A и §56.

---

## 37. Resource model — FROZEN

Host identity = immutable HostID, не hostname/IP/MAC.

GPU identity = stable DeviceID (PCI/GPU UUID based), не GPU0/GPU1.

Rules:
- one RUNNING CPU workload per Host;
- one DeviceID cannot belong to two RUNNING workloads;
- CPU + GPU workloads may coexist;
- different GPUs may run different workloads;
- STOPPED workload не резервирует active resource.

---

## 37A. Hardware replacement and validation before START — FROZEN

Перед каждым runtime START ResolvedExecutionPlan валидируется против fresh hardware inventory и capabilities. GPU identity не может основываться только на ordinal вроде `GPU0`.

Если GPU заменена:

- прежний DeviceID становится missing;
- новое hardware получает/использует собственный stable DeviceID;
- прежние resolved tuning/settings не применяются к нему молча;
- Profile compatibility, Package capabilities, driver/capabilities и applicable HostProfileSettings разрешаются заново до START.

Если тот же physical GPU перемещён в другой PCI slot, но stable hardware identity надёжно доказывает, что это то же устройство, Le0xFarm должен по возможности сохранить identity.

Любое hardware identity change требует re-resolution runtime plan. Невозможная или неподтверждённая compatibility блокирует START с видимой причиной.

---

## 38. Reconciliation ownership — FROZEN

Ownership определяется explicit metadata:
- ExecutionID;
- WorkloadID;
- DesiredGeneration;
- ResolvedHash;
- HostID;
- ResourceClaim.

Не PID/executable/hostname.

Safe replacement:

```text
STOP exact old ExecutionID
→ confirm absent
→ START new ExecutionID
```

---

## 39. Status — FROZEN

Минимум:
- OFFLINE;
- IDLE;
- STARTING;
- MINING;
- DEGRADED;
- ERROR.

ONLINE ≠ MINING.

Для hash-based workload `MINING` требует process + fresh telemetry + relevant connectivity + positive hashrate.

Для workload без традиционного hashrate этот критерий заменяется workload-specific fresh useful-work evidence, определённым Adapter/Provider. Le0xFarm не создаёт фиктивный hashrate; `WORKING` может использоваться как нейтральная presentation state для non-hash workload при сохранении stable internal status semantics.

Если expected resource participation нельзя доказать → DEGRADED.

---

## 40. Watchdog / Retry — FROZEN

Agent имеет bounded watchdog.

Terminal FAILED блокирует текущую generation.

Explicit Retry:
- new generation;
- new ExecutionID;
- new immutable snapshot.

Transport uncertainty ≠ terminal failure.

---

## 41. Dependencies / GPU tuning — PLANNED

Le0xFarm должен объяснять missing dependencies.

Не обновлять автоматически бездумно:
- kernel;
- BIOS;
- firmware;
- NVIDIA driver;
- CUDA;
- ROCm;
- DKMS.

GPU tuning сначала минимальный safe set:
- Power Limit.

Дальше capability-driven.

---

## 42. Logging — FROZEN / REQUIRED

Все компоненты пишут structured logs:
- Controller;
- Agent;
- Noda;
- Helper;
- Brain integration;
- Relay.

Correlation IDs где применимо:
- FarmID;
- HostID;
- WorkloadID;
- ExecutionID;
- PackageID;
- ServiceID;
- ConnectionEpoch.

Levels:
- DEBUG;
- INFO;
- WARN;
- ERROR.

Не логировать plaintext:
- seed/mnemonic;
- private wallet keys;
- Recovery Private Key;
- passwords;
- enrollment tokens;
- operational API keys/tokens и personal integration credentials;
- private signing keys.

Нужны redaction и rotation.

One-time credentials не идут в обычный operational log stream.

---

## 43. Audit/history — PLANNED

Нужно уметь ответить:
> почему Host вчера переключился с ZEPH на XMR?

Audit:
- actor;
- time;
- previous/new revision;
- runtime action;
- result.

Желательно tamper-evident.

---

## 43A. Diagnostics and supportability — FROZEN DIRECTION / PLANNED

Le0xFarm должен позволять диагностировать проблему без выдачи developer/support универсального root-доступа.

Нужны:

- unified event/timeline view;
- correlation between config change, runtime action, telemetry and incident;
- `le0x doctor`;
- one-click/one-command diagnostic bundle;
- deterministic incident/report ID;
- version/build/environment summary;
- comparison of healthy vs broken Host where useful.

Support bundle должен:

- быть sanitized/redacted;
- иметь preview перед export;
- исключать secrets;
- минимизировать topology/account leakage;
- создаваться только с user consent.

Basic diagnostics/support bundle относятся к CORE/FREE.

Advanced AI diagnosis может быть Pro, но core diagnosis не зависит от Brain.

---

## 44. Error model — FROZEN DIRECTION

Typed errors:
- stable code;
- localization key/message;
- parameters/details;
- suggested fix;
- logs reference.

Не один `exit status 1`.

---

## 45. Installer / systemd — PLANNED

Идея:

```text
sudo le0x install
```

Installer:
- role selection;
- `le0x` user;
- binaries;
- directories/permissions;
- systemd;
- PKI/bootstrap;
- helper;
- doctor/preflight.

Services:
- le0x-controller.service
- le0x-agent.service
- le0x-noda.service
- le0x-helper.service

Ubuntu 22.04/24.04/26.04 LTS, amd64 first.

---

## 45A. First-run, enrollment and lifecycle UX — FROZEN DIRECTION / PLANNED

First-run должен быть guided и resumable.

Идеальный onboarding не заставляет новичка вручную понимать все внутренние domain objects.

Концептуальные этапы:

1. Create new Farm / join existing Farm.
2. Choose roles: Controller / Agent / both / Noda where needed.
3. Language/timezone.
4. Detect existing mining/services.
5. Establish Owner authentication and Farm trust.
6. Enroll first Host.
7. Detect hardware/capabilities.
8. Choose mining goal/profile/payout/pool.
9. Review exact effect and privilege requirements.
10. Start.
11. Prove useful mining.
12. Configure essential alert.
13. Create/test Farm backup path.

Enrollment должен использовать short-lived/expiring grants/tokens и поддерживать batch enrollment later.

Нельзя помещать shared persistent HostID/private identity в cloned image.

Host lifecycle должен учитывать:

- awaiting approval;
- connected;
- inventory ready;
- unmanaged activity detected;
- approved/managed;
- reinstall/replacement;
- de-enrollment/decommission.

---

## 46. Profitability/autoswitch — PLANNED

Policy layer работает выше Desired State.

```text
Profitability policy
→ Desired State mutation
→ Reconciler
```

Не direct Agent control.

---

## 47. Modularity / easy future changes — FROZEN / CRITICAL

Le0xFarm должен быть модульным.

Концептуальные domains:
- identity;
- farmmodel;
- farmconfig;
- reconcile;
- controllernet;
- agentnet;
- minerruntime;
- miners;
- packages;
- wallets;
- portfolio;
- miningaccounting;
- marketintelligence;
- secrets;
- noda;
- helper;
- brain;
- relay;
- telemetry;
- audit.

New miner → Adapter.
New wallet → WalletProvider.
New node → NodeProvider/ServiceAdapter.

Coin/miner-specific logic не размазывается по core.

---

## 48. Versioned evolution / testability — FROZEN

Нужны:
- protocol versions;
- schema versions;
- deterministic DB migrations;
- capability negotiation;
- fail-closed incompatibility.

Historical snapshots требуют version-aware decoder/hash semantics.

Concurrency safety проверяется adversarial/barrier-controlled tests, не только `-race`.

---

## 49. Recovery domains — FROZEN

Separate:
- Farm Recovery;
- Wallet Recovery;
- Farm PKI/CA;
- Brain signing hierarchy.

Ключи не переиспользуются между доменами.

---

## 49A. Proven recovery — FROZEN DIRECTION / PLANNED

Факт наличия backup-файла не равен доказанной recoverability.

Farm Recovery и Wallet Recovery должны иметь independently testable workflows.

Нужно различать:

- backup created;
- integrity checked;
- off-machine copy exists;
- restore preview;
- successful isolated restore exercise;
- last recovery-test date.

Планируются:

- recovery-kit export;
- backup health indicator;
- restore wizard;
- isolated restore test;
- explicit Recovery Point / Recovery Time expectations.

Особенно важно при restore старого Controller backup:

**FROZEN:** восстановленный старый Desired State не должен молча применяться к более новой physical reality. После disaster restore Controller сначала должен заново observe reality и показать impact потенциальной reconciliation.

Automatic Controller backup policy:

- запускать hourly, только если relevant persistent Controller state изменился;
- хранить 24 hourly backups;
- хранить 7 daily backups.

Controller backups и Wallet Recovery backups — разные domains и lifecycle.

Restore original Controller:

```text
restore Controller identity/configuration
→ start Controller
→ reconnect Agents
→ obtain fresh inventory and actual executions
→ compare restored Desired State with current Actual State
→ surface discrepancies
→ reconcile safely
```

Если restored Desired и Actual совпадают, normal operation может продолжиться. Если Farm materially изменилась после backup, Controller показывает differences до destructive/change-producing convergence и не применяет старый Desired State вслепую.

Для completely new Controller без восстановленной original Controller identity initial v1 recovery может требовать:

- вручную остановить old mining;
- enroll Hosts в новый Controller;
- explicit start desired workloads.

Unsafe automatic adoption при таком recovery запрещён.

---

## 50. Identity lifecycle — OPEN

До Installer/Noda/Entitlements формально решить:
- Agent и Noda на одном physical Host делят HostID;
- component credentials отдельные;
- reinstall;
- cloned disk;
- host replacement;
- Controller migration;
- cert renewal/revocation;
- key rotation;
- entitlement binding after key change.

Stable HostID ≠ permanent certificate.

---

## 51. Chain/network/asset identity — FROZEN REQUIREMENT / OPEN MODEL

**FROZEN:** ticker недостаточен. Asset identity должна различать network/chain/contract, forks, mainnet/testnet и migrations и не сливать same-ticker assets (§27C).

**OPEN:** exact identity/schema/canonicalization model, включая:
- mainnet/testnet;
- forks;
- duplicate tickers;
- Service instance identity;
- endpoint auth;
- data ownership/retention.

---

## 52. Shared privileged preparation — OPEN

Hugepages/MSR/GPU settings могут быть host-global/shared.

Нужно определить:
- ownership;
- authorization scope;
- conflict rules;
- reference counting;
- restoration.

---

## 53. Scale / long-running Farm — DESIGN CONSTRAINT

Не проектировать только на 5 машин.

Нужны:
- indexed scoped queries;
- bounded history/retention;
- version-aware historical decoding;
- isolation damaged historical data.

Не позволять одному старому повреждённому snapshot ломать unrelated reconciliation.

---

## 53A. Site concept and multi-site — FROZEN DIRECTION / PLANNED

Для remote/multi-site Farm нужен first-class **Site** concept, а не только UserGroup.

Site может содержать:

- display name;
- timezone;
- connectivity state / last reliable contact;
- bandwidth/update preferences;
- physical location labels;
- site-wide incidents;
- optional power/circuit metadata;
- node/service placement context.

Один authoritative Controller сохраняется; Site не становится скрытым локальным master.

Нужно учитывать:

- overlapping private IP ranges;
- network partitions;
- per-site timezone/DST;
- reconnect storms;
- staggered startup after site-wide power loss;
- bandwidth-sensitive package/update rollout.

Initial tested scale targets — это capability/SLO, а не licensing limits.

Рекомендуется публиковать фактически протестированный envelope отдельно от принципа «no artificial Host limit».

---

## 54. Reboot/crash recovery — PLANNED / MUST DESIGN

Нужно определить:
- surviving miner processes;
- runtime files;
- ownership recovery;
- duplicate prevention;
- reconnect/reconciliation after reboot.

---

## 55. Product UX principle — FROZEN

Le0xFarm должен экономить время.

Пользователь не обязан знать:
- systemd;
- SSH;
- miner flags;
- pool login syntax;
- node CLI;
- wallet internals.

UI показывает:
- Desired;
- Observed;
- current action;
- reason for waiting/error;
- suggested fix.

---

## 55A. Daily-use UX — FROZEN DIRECTION

Главный dashboard должен сначала отвечать на пять вопросов:

1. Что сейчас действительно сломано?
2. Соответствует ли actual mining моему Desired State?
3. Есть ли idle/wasted resources?
4. Есть ли риск для funds/recovery/security?
5. Где требуется моё решение?
6. Какие known, unpaid и locked mining assets требуют внимания и насколько fresh эти данные?

Основные блоки интерфейса:

- Needs Attention / incidents;
- Desired vs Observed;
- Production / useful-work evidence;
- Capacity / idle/unavailable resources;
- Safety / backups/certs/revocations/disk pressure;
- Recent changes;
- Portfolio/accounting с separate spendable, unpaid/pending и immature/locked state;
- optional economics с explicit estimate/uncertainty.

Нельзя бессмысленно суммировать hashrates разных algorithms в один misleading «Total Hashrate».

Fast actions:

- open incident/logs;
- acknowledge;
- maintenance hold;
- preview switch;
- exact scoped start/stop.

---

## 55B. Product exit, portability and uninstall — FROZEN USER RIGHT / PLANNED

Пользователь должен иметь возможность безопасно покинуть Le0xFarm или пережить исчезновение developer infrastructure.

Нужны/планируются:

- versioned configuration export;
- safe Profile sharing без payout/secrets leakage;
- Host de-enrollment;
- Controller replacement/migration;
- clean Agent/Noda/Controller uninstall;
- explicit data-preservation choices;
- orphan process/service/helper detection;
- documented independent recovery.

По умолчанию destructive uninstall не должен удалять wallets или blockchain data без отдельного подтверждения.

После uninstall не должны оставаться скрытые managed watchdogs/processes/privileged preparation, о которых пользователь не знает.

**FROZEN:** shutdown developer business/Brain не должен лишать пользователя доступа к local config, wallets, recovery/export и core local management.

---

## 55C. Supported-product contract — PLANNED BEFORE PUBLIC RELEASE

Публичный Le0xFarm должен честно различать:

- supported;
- tested;
- experimental;
- unsupported/known limitation.

Нужна publishable compatibility/support matrix по:

- OS/architecture;
- miners;
- hardware classes;
- pools;
- nodes;
- wallet providers;
- operations (install/telemetry/update/rollback/etc.).

Нужны:

- security update policy;
- vulnerability-reporting process;
- capability maturity labels;
- known limitations;
- third-party artifact provenance/license records.

Собственная лицензия Le0xFarm не отменяет лицензии redistributed miners/nodes/wallet binaries.

---

## 55D. Technical MVP product definition — FROZEN TARGET

TECHNICAL MVP должен пройти узкий, но полный local Controller/Agent operating loop для testing и dogfooding:

1. repeatable Controller/Agent installation;
2. secure component ownership/enrollment;
3. observe existing Host without disruption;
4. block conflicting unmanaged process and perform deliberate cutover;
5. configure public payout WalletRef + Pool/Profile;
6. use reusable Profile + understandable Host-specific settings;
7. start/stop/switch exact CPU/GPU resources;
8. show Desired, Observed, freshness, action, blocker;
9. prove workload-specific useful work beyond process existence;
10. generate an essential alert without Brain;
11. Maintenance Hold / safe resume;
12. survive Controller restart and Agent reconnect without duplicate runtime;
13. run through real test/dogfood acceptance for the supported workload.

Managed wallet creation, full Brain intelligence, Telegram convenience, industrial power control и advanced profitability не обязательны для TECHNICAL MVP.

---

## 55E. Pavel daily-use / Pro beta gate — FROZEN TARGET

До того как Pavel перестаёт полагаться на прежние manual mining workflows, Le0xFarm должен дополнительно обеспечивать:

1. secure local Owner authentication and routine RU/EN UX;
2. mature daily Controller/Agent operation, Host reboot recovery and essential local incidents;
3. safe migration for the supported existing Farm;
4. Maintenance Hold, safe resume and understandable bulk-operation results;
5. Farm backup, restore exercise and Controller recovery workflow;
6. supported component update with tested rollback path;
7. config export and clean managed-host removal;
8. Brain discovery of new mining projects;
9. repository/documentation/release analysis;
10. verified Package/Profile recipe generation and local validation;
11. Noda topology end-to-end when required by a selected workload;
12. managed wallet subsystem when required by a selected new project;
13. remote/Telegram convenience through normal Controller authorization;
14. workload-specific useful-work validation and actionable diagnostics.
15. если используется contribution-based Pro entitlement — первый полный PRO ABUSE / ENTITLEMENT RED-TEAM GATE (§36B), включая fixes и повторную independent adversarial verification.
16. local portfolio/mining accounting с historical/dormant asset awareness, lifecycle/no-double-counting и honest UNKNOWN/stale/provider states (§27C–§27F).
17. Brain project/market intelligence и proactive deduplicated Telegram portfolio alerts с provenance, verified links и approximate-value language (§27G–§27H).
18. full synthetic portfolio/market и mining-accounting lifecycle acceptance gate §27I.

Le0xFarm к этой стадии отвечает в одном месте:

- какие projects mined historically и какие WalletRefs получали rewards;
- какие known balances, unpaid и immature/locked rewards остались;
- какие balances UNKNOWN и почему;
- какова approximate current value и uncertainty;
- какие old assets became tradable, listed/delisted или require swap/migration/wallet upgrade;
- какой deadline/action user рискует пропустить.

Не требуется automatic balance provider для every obscure blockchain. Unsupported provider явно говорит `UNSUPPORTED` / `UNKNOWN`, а не zero.

Profitability/autoswitch может продолжать постепенно созревать. Pro beta уже должна позволять осмысленно находить, проверять и запускать новые mining projects через canonical safe path.

---

## 55F. Public release gate — PLANNED

До внешних пользователей дополнительно нужны:

- first-run usability testing на людях, которые не являются авторами проекта;
- clear supported matrix;
- owner recovery и базовое role separation;
- strong remote authentication;
- batch enrollment / cloned identity handling;
- safe import/export/uninstall/Controller replacement;
- incident-grouped essential alerts;
- tested off-machine Farm backup/restore;
- sanitized support workflow;
- update compatibility/security response policy;
- third-party licensing/provenance review;
- storage budgets/retention;
- tested multi-site behavior, если оно рекламируется;
- accessibility basics и полное RU/EN покрытие;
- vendor-disappearance/end-of-life plan;
- sustainable support/operating-cost model.
- portfolio/provider compatibility and maturity visibility;
- rate-limit/retry/provider-failure behavior, balance/quote freshness и network/ticker ambiguity handling;
- market-source provenance/confidence, false-positive correction и duplicate-event suppression;
- dormant-asset retention/export, privacy и Core availability during Brain outage;
- Telegram outage/retry, secret redaction/rotation/revocation и exchange/API permission-scope visibility;
- public Open Core can build/run without private Brain repositories, while official Pro dependencies are explicit (§28A);
- private signing/entitlement/infrastructure secrets absent from public source and binaries;
- clear official-vs-fork branding and Core license/service-entitlement distinction;
- Free export/recovery and user-owned data remain available without Pro.

До public Pro отдельно закрыть contribution accounting/arbitration/privacy/suspension и independently review official Brain authorization against hostile forked/patched clients, replay/cloning/impersonation, fake evidence, direct API abuse, downgrade и Brain-compromise containment.

После их design и implementation PUBLIC PRO обязан повторно пройти PRO ABUSE / ENTITLEMENT RED-TEAM GATE (§36B) против complete production system. Overcharge и fee bypass одинаково блокируют release.

До public managed-wallet support отдельно закрыть wallet threat model, crash-safe provisioning, working-wallet protection и recovery testing.

---

## 56. OPEN PRODUCT QUESTIONS

Этот раздел — осознанный backlog решений. Они не должны молча определяться реализацией.

### Resolved by approved v1.2 decisions

Следующие прежние OPEN questions закрыты и сохранены здесь для traceability:

- первый real user и initial target operator → §1B;
- различие TECHNICAL MVP, PAVEL DAILY-USE / PRO BETA и PUBLIC RELEASE → §1A, §55D–§55F;
- payout-only first против managed-wallet phasing → §20A;
- HostProfileSettings precedence и отсутствие arbitrary DesiredWorkload tuning overrides в initial MVP → §6;
- normal bulk command freeze target set против continuing POLICY → §10B;
- generic useful-work definition beyond `hashrate > 0` → §12A и §39;
- STOP confirmation и Maintenance enter/exit suppression semantics → §11A;
- initial Owner authentication и remote second-factor direction → §10A;
- restored Controller reconciliation против newer physical reality → §49A;
- обязательная независимая symmetric anti-bypass/anti-overcharge verification перед Pro Beta и PUBLIC PRO → §36B, §55E, §55F; unfinished contribution mechanics остаются OPEN ниже.
- ticker-only asset identity запрещена; exact chain/network/contract model остаётся OPEN → §27C, §51;
- Core portfolio/mining-accounting ownership, reward lifecycle и no-double-counting → §27C–§27F;
- proactive Brain/Telegram market intelligence является PRO, а local user-owned portfolio/accounting — CORE/FREE → §27H, §33, §34;
- `.env` является local SecretProvider/input, а не canonical secret domain → §27A–§27B;
- open-source Core/private official Brain source and service-entitlement boundary → §28A;
- official Brain treats public/patched Controller as untrusted entitlement client; hostile fork attacks входят в mandatory red-team gate → §28A, §36B.

### Product scope / user

2. Какие exact workloads обязаны войти в PAVEL DAILY-USE / PRO BETA support envelope?
4. Будет ли Le0xFarm когда-либо sign/broadcast transactions?
5. Какие practical scale/SLO обещания публикуются для 1/20/100/1000 Hosts?

### Profiles / targeting / operations

7. Что происходит при изменении detected capability: новая generation или block current intent?
9. Semantics cancel partial bulk operation.
10. Temporary Profile semantics при новом user change до expiry.
11. Safe policy/schedule priority model.
12. Emergency actions during partition и exact authorization для override Maintenance Hold.

### Mining proof / Pools

13. Exact Adapter/Provider evidence schema и thresholds для каждого supported mode.
14. Как presentation различает MINING/WORKING для heterogeneous workloads без несовместимых aggregate metrics?
15. Pool failover semantics, priority, region and TLS policy.
16. Pool-account login vs payout address model.
17. Optional pool API accounting/balance verification boundary.

### Wallets / secrets

18. Working wallet encryption/isolation/unlock lifecycle.
19. Wallet provisioning state machine details.
20. Immutable/versioned wallet BackupID naming.
21. RecoveryKeyID/BackupID metadata model.
22. Exact managed-wallet custody scope.
23. Transaction authority, если она когда-либо появится.
24. Operational secret store/unlock/rotation model.
25. Web Wallet automation boundary.
26. Offline wallet-backup transport.
27. Hardware/watch-only wallet support scope.

### Contribution / Pro

28. Contribution arbitration/effective execution layer.
29. Debt/preemption/suspension/grace semantics.
30. Maximum uninterrupted developer session.
31. Controller outage behavior during active dev mining.
32. Mixed/dual mining accounting.
33. Hardware replacement/debt identity behavior.
34. Pool-side verification privacy minimum.
35. Pro entitlement transfer after Controller recovery/key rotation.
36. Sustainable Brain/Relay/catalog/support cost model.
74. Exact official Brain entitlement protocol, receipt/token format и contribution-proof mechanism.
75. Brain API stability promise и whether third-party Brain endpoints are ever supported.
76. Commercial pricing beyond already FROZEN contribution rules, Brain service continuity и entitlement recovery UX.
77. Possible future enterprise/on-prem or licensed private Brain distribution.

### Security / trust

37. Independent Controller validation of Brain-delivered executable artifacts.
38. Brain signing-key revocation/compromise recovery.
39. Identity/certificate lifecycle.
40. Duplicate/cloned HostID recovery UX.
41. Post-Owner RBAC granularity, TOTP enrollment/recovery UX и exact session lifetimes.
42. Security incident response and vulnerability disclosure process.
43. Package/artifact revocation while currently running.
44. Support technician temporary access model.
45. Support bundle redaction/consent details.

### Noda / chain / privileged operations

46. Mainnet/testnet/fork/service identity.
47. Shared/per-host node failover policy.
48. Node destructive repair/reindex/reset approval semantics.
49. Shared privileged preparation ownership/reference-count/restoration.
50. Automatic node placement recommendation policy.

### Recovery / lifecycle / operations

51. Farm disaster-recovery workflow.
52. Recovery Point / Recovery Time targets.
53. Exact review/confirmation UX для discrepancies между restored Desired и newer physical reality.
54. Reboot/crash ownership recovery.
55. Controller migration/replacement UX.
56. Agent uninstall/reinstall/orphan cleanup.
57. Vendor/project end-of-life workflow.
58. Maintenance windows/planned downtime.
59. External dead-man check for Controller.
60. Multi-site/network partition guarantees.
61. Clock skew/time sync policy.
62. Certificate expiry/revocation UX.
63. Site timezone/DST semantics.
64. Site-wide staggered startup after outage.

### Data / UI / support

65. Audit retention/storage.
66. Telemetry retention/aggregation.
67. Log retention/storage budgets.
68. Search/filter/pagination/topology UX at scale.
69. Accessibility completeness.
70. Additional languages beyond RU/EN.
71. Data/privacy consent for Brain/analytics/notifications.
72. Export/import versioning and safe shared Profile format.
73. Licensing/provenance handling per redistributed artifact.

### Portfolio / mining accounting / market intelligence

78. Exact Asset/Network identity schema and canonicalization/provider mapping.
79. BalanceProvider/AccountingProvider interfaces, provider precedence и reconciliation/correlation algorithm.
80. Ledger/event schema, idempotency identifiers и accounting retention limits.
81. Provider-specific polling intervals, forced-refresh policy, quotas и backoff.
82. Market quote selection, historical price retention, liquidity-adjusted valuation и confidence formula.
83. Dormant asset/portfolio retention and explicit removal lifecycle.
84. Exact MarketEvent, maturity/unlock event lifecycle, deduplication thresholds и Telegram retry policy.
85. Trusted-domain/catalog and false-positive correction workflow.
86. Transaction-aware sale/transfer/outflow reconciliation; automatic trading остаётся separately prohibited без new architecture decision.

### Open Core / private Brain / legal boundary

87. Exact Brain deployment topology, hosting, database, queue, AI model/provider и internal operations.
88. Exact Brain API compatibility/versioning promise и alternative-backend policy.
89. Terms of Service и official service commercial/legal wording.
90. Trademark/brand policy legal text для official names, logos, forks и modified builds.
91. Exact private key-management/HSM operational process beyond frozen offline-root/online-key boundary.

---

## 57. Product-gap audit requirement — FROZEN PROCESS DECISION

До дальнейших крупных product milestones нужно отдельно проверить **не код, а полноту самой идеи Le0xFarm как готового продукта**.

Главный вопрос:

> Если забыть про текущий код вообще: достаточно ли полно мы придумали Le0xFarm как готовый продукт для реального пользователя?

Независимый Product-Gap Audit должен подробно изучить существующие зрелые продукты и сравнить:
- onboarding;
- deployment;
- fleet enrollment;
- monitoring;
- grouping;
- automation;
- failover;
- backup/recovery;
- wallet UX;
- security;
- remote management;
- update rollout/rollback;
- diagnostics;
- support;
- logging/audit;
- multi-site;
- policies;
- profitability;
- maintenance;
- mobile/Telegram UX;
- monetization;
- operational safety.

Цель — найти идеи, которые:
- ускоряют первый запуск;
- уменьшают ручной труд;
- повышают безопасность;
- уменьшают шанс потери средств;
- ускоряют диагностику;
- упрощают управление большой Farm;
- улучшают recovery;
- уменьшают требования к техническим знаниям пользователя.

Нужно изучать не только mining-specific продукты, но при необходимости и mature fleet/orchestration/security products для полезных паттернов:
- enrollment;
- RBAC;
- policy;
- rollout;
- secrets;
- audit;
- disaster recovery;
- support bundles;
- remote access.

Изучаются **идеи, workflows и UX-паттерны**, а не копируется чужой код.

Минимальные competitor/reference directions:
- Hive OS;
- minerstat;
- Awesome Miner;
- Foreman;
- RaveOS;
- mmpOS;
- XMRigCC;
- RainbowMiner;
- Hermes;
- другие актуальные mining farm managers, найденные аудитором самостоятельно.

---

## 58. Rejected / non-canonical paths

Без отдельного пересмотра НЕ использовать:
- SSH как runtime transport;
- Agent с `NOPASSWD: ALL`;
- Agent как universal root shell;
- Brain с direct shell/control access к Agent;
- wallet secrets в Brain;
- silent automatic Wallet Recovery key replacement;
- artificial Free Host/GPU limits;
- developer contribution во Free;
- command queue вместо Desired State;
- Host identity по hostname/IP/MAC;
- arbitrary shell Profile;
- automatic kill/adoption unknown processes;
- silent overwrite user-modified Profile;
- Relay terminating application trust;
- Noda как единственное место хранения recoverable wallet.
- ticker-only asset identity и silent merge same-ticker networks/contracts;
- unknown/provider-unavailable balance как fabricated zero;
- independent balance snapshots, дважды counting один reward при maturity/payout/wallet receipt;
- `.env` как canonical secret domain или storage wallet/recovery/PKI private keys;
- exchange monitoring credential с trade/withdrawal authority by default;
- automatic trading/withdrawal/transaction signing без отдельного architecture/security decision;
- Discord user-token/self-bot impersonation и unverified social/AI URLs в Telegram;
- locally editable `pro=true` как proof official Pro entitlement;
- secrecy Brain wire format как security boundary;
- private developer signing/entitlement/infrastructure secrets в public source или distributed binaries;
- private Brain как повод скрыть или cripple user-owned Core data/runtime.

---

## 59. Главный критерий будущих решений

Каждая новая функция должна отвечать:

Можно ли её реализовать, сохраняя:
- Controller authority;
- Desired/Observed;
- typed runtime;
- Agent non-root;
- strict trust boundaries;
- Brain non-authoritative runtime role;
- Wallet Recovery security;
- modular adapters/providers;
- Free local independence;
- multilingual UX;
- safe failure/recovery?

Если нет — сначала отдельное архитектурное решение, потом код.

---

## 60. Итоговый замысел

Пользователь должен установить Le0xFarm, добавить машины и управлять ими как одной Farm.

Команда:

> «Переключи все 5090 на PBC»

должна безопасно превратиться в:

```text
parse intent
→ resolve group
→ validate Profile
→ mutate Desired State
→ reconcile
→ show result
```

без ручного SSH на каждую машину.

При этом compromise одного Agent, Relay или даже Brain не должен автоматически давать root/full-wallet compromise всей Farm.

Новые miners, coins, nodes, wallet providers, hardware, languages и Brain features должны обычно добавляться через adapters/providers/capabilities, а не через переписывание ядра.

---

**END OF CANONICAL PRODUCT & ARCHITECTURE**
