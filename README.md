# Le0xFarm

Le0xFarm — проект системы управления оборудованием, профилями майнинга и сервисами фермы.

Планируемые компоненты:
- **Le0xAgent** — локальный исполнитель на хосте: инвентаризация и применение заданного состояния.
- **Le0xController** — управление фермой и координация агентов.
- **Le0xNoda** — компонент для работы с нодами и связанными сервисами.
- **Le0xBrain** — будущая аналитика и автоматизация решений.

Текущий этап — **M3 Generic Miner Runtime + XMRig adapter**. Существующий M1 transport использует
persistent bidirectional gRPC через TLS 1.3 и mutual TLS; plaintext доступен только при
явном `--insecure-dev`. Agent сохраняет identities и PKI, собирает Linux inventory и
может выполнять resolved process plans. M3 добавляет generic miner adapter contract,
проверяемое локальное хранилище packages и первый adapter XMRig. Le0xNoda/Le0xBrain,
DesiredState persistence, Controller-to-Agent package distribution и systemd не реализованы.

## M3 — Generic Miner Runtime и XMRig 6.26.0

`internal/minerruntime` содержит registry и общий MinerAdapter contract. Adapter объявляет
capabilities, проверяет generic MinerSpec, готовит resolved ExecutionPlan, предоставляет
источник нормализованной MinerTelemetry и очищает свои runtime resources. Supervisor,
gRPC transport и Controller command path не знают о формате XMRig config или HTTP API.
Тестовый второй adapter без HTTP API доказывает эту границу. Для добавления SRBMiner,
Rigel или другого miner нужны adapter package, manifest/catalog entry, регистрация и
adapter-specific tests; Supervisor, protocol lifecycle и agentnet менять не требуется.

`internal/packages` устанавливает уже локально доставленные tar.gz artifacts. Store сам
проверяет SHA-256 до extraction, отклоняет absolute/traversal paths, links и special files,
публикует install атомарно и проверяет hashes при каждом lookup. Agent не скачивает
packages из Internet. В M3 Controller distribution ещё отсутствует; acceptance archive
доставляется вручную после проверки на development host.

Первый production adapter использует только официальный XMRig 6.26.0 static amd64 asset
с SHA-256 `fc6f8ae5f64e4f17481f7e3be29a1c56949f216a998414188003eae1db20c9e5`.
Manifest фиксирует upstream repository/release URL и штатный XMRig donate level 1%.
Opaque PackageID `package_5f5d8ae2b63ce1001aa0a3b8a2a9b29a` идентифицирует
семейство XMRig и остаётся тем же для будущих версий; `6.26.0` является отдельной
version dimension. Store layout — `packages/<PackageID>/<Version>/`.
Adapter создаёт для каждого ExecutionID отдельный `runtime/<ExecutionID>/xmrig-config.json`
с правами 0600 и запускает verified executable напрямую через Supervisor. XMRig API
выделяет отдельный порт и bind только `127.0.0.1`; localhost HTTP client отключает proxy,
ограничивает response одним MiB и использует timeout. Port reservation имеет небольшой
bind-close-start race, потому что XMRig сам открывает listener; collision приводит к
обычной startup/runtime ошибке, не к использованию внешнего bind address.

Telemetry polling по умолчанию выполняется раз в 3 секунды, startup grace равен 45
секундам, stale threshold — 10 секунд. XMRig `/2/summary` нормализуется в общие поля
version, algorithm, short/medium/long/highest hashrate, shares/results, pool connection,
latency, uptime и optimization status. Process RUNNING сам по себе не означает MINING.
MINING требует mining mode, healthy API, connected pool и hashrate > 0. STRESS и
BENCHMARK не влияют на общий Agent status. Для активных MINING executions используются
IDLE, STARTING, MINING, DEGRADED и ERROR с deterministic safety precedence
ERROR > DEGRADED > STARTING > MINING > IDLE. STOPPED history не влияет на status.
XMRig STRESS не является полностью offline workload: штатная реализация
XMRig может подключаться к внешнему upstream stress service `randomx.xmrig.com:443`.
Это не пользовательский payout pool mining; payout wallet в STRESS не передаётся.

Для MINING Controller передаёт resolved `MiningEndpoint`: `address`, явный TLS intent,
точный miner-facing `user`, потенциально чувствительный optional `password` и optional
отдельный `worker`. Agent не склеивает user и worker и не нуждается в persistent
Pool/Wallet storage. Если конкретный pool кодирует worker внутри login, Controller
передаёт уже готовый exact user и оставляет отдельный worker пустым. Password существует
только в in-memory protobuf command внутри защищённого mTLS stream и в runtime config
0600; command, config, environment и credential values не логируются и не сохраняются
в новой базе. Pool address, algorithm, public payout/login и worker считаются публичными.

Huge pages только запрашиваются в config и наблюдаются; M3 не меняет sysctl/GRUB.
MSR capability проверяется read-only, а XMRig rdmsr/wrmsr отключены: sudo, modprobe и
kernel writes отсутствуют. Automatic certificate renewal, package signing и restoration
executions после полного Agent restart остаются следующими этапами.

Development/test Controller action проходит через существующий authenticated mTLS stream
и требует точный target Agent:

```sh
le0x-controller --listen 0.0.0.0:50051 \
  --dev-runtime-action start-miner \
  --target-agent agent_0123456789abcdef0123456789abcdef \
  --execution-id execution_0123456789abcdef0123456789abcdef \
  --miner-adapter xmrig \
  --package-id package_5f5d8ae2b63ce1001aa0a3b8a2a9b29a \
  --package-version 6.26.0 --miner-mode STRESS \
  --algorithm rx/0 --cpu-threads 2 --restart-policy ON_FAILURE
```

## M2 — Agent Runtime Supervisor

`internal/runtime/supervisor` управляет несколькими процессами по `ExecutionID`.
START, STOP, RESTART и GET_EXECUTIONS идут по существующему authenticated
AgentControl stream. Supervisor создаётся один раз на lifetime Agent, поэтому разрыв
Controller connection не останавливает workload, а reconnect сообщает тот же PID и
ExecutionID. Нормальный restart всего Agent пока не восстанавливает executions.

ExecutionPlan передаёт абсолютный clean executable path, отдельный argv, environment,
working directory и restart policy. Процесс запускается напрямую через `os/exec`, без
shell и interpolation, в отдельной Linux process group с `Pdeathsig`. STOP посылает
SIGTERM группе, ждёт 5 секунд и при необходимости посылает SIGKILL. Shutdown Agent
останавливает supervised children тем же механизмом. stdout и stderr хранятся раздельно
в bounded in-memory tails по 64 KiB; значения environment не логируются.

При `ON_FAILURE` неожиданный exit перезапускается с backoff 1, 2, 4, 8, 16 секунд до
максимума 30 секунд. Пять crashes за 10 минут переводят execution в FAILED с
`PROCESS_CRASHED`. Explicit STOP отменяет pending restart. `NEVER` оставляет неожиданно
завершившийся execution в CRASHED.

Для acceptance доступен явный development/test CLI Controller, который использует тот
же mTLS stream и не создаёт дополнительного listener:

```sh
le0x-controller --listen 0.0.0.0:50051 \
  --dev-runtime-action start \
  --target-agent agent_0123456789abcdef0123456789abcdef \
  --execution-id execution_0123456789abcdef0123456789abcdef \
  --executable /bin/sleep --execution-arg 300
```

Действия: `start`, `stop`, `restart`, `get`. Этот механизм нужен до появления
persistent DesiredState/Controller API и не обходит authentication.

## M1.3 — persistent Controller identity

ControllerID and FarmID are now persisted independently from the Controller runtime in
`internal/controlleridentity`. First initialization requires the explicit `--init` flag;
normal restart only loads an existing identity. The file contains only `schema_version`, `controller_id`
and `farm_id`; storage schema version is 1. The default Linux directory is
`$XDG_DATA_HOME/le0xfarm/controller` or `$HOME/.local/share/le0xfarm/controller`.
`LE0X_CONTROLLER_DATA_DIR` overrides it with an exact development/test directory.
The directory is 0700 and `identity.json` is 0600. Creation is atomic and existing or
corrupted files are never silently replaced. A missing or future schema version is a
typed `CONFIG_CONFLICT`. Controller identity is independent of hostname, network
addresses and listen address; deleting storage makes normal startup fail. A new identity
is created only by an explicit `--init` after the loss is intentional.
Controller runtime receives both IDs explicitly and uses them in ControllerHello.

## M1.2 — first Agent ↔ Controller connection

`le0x-controller` и network mode `le0x-agent --controller HOST:PORT --insecure-dev` теперь
поднимают первый persistent bidirectional gRPC stream на базе `AgentControl.Connect`.
`--insecure-dev` — только development/test механизм и никогда не является transport по
умолчанию. Без него Controller отказывается запускать plaintext, а Agent не подключается;
production architecture остаётся persistent gRPC + mTLS и будет реализована отдельно.

Controller загружает persistent ControllerID/FarmID; первый запуск выполняется с `--init`.
После handshake он отправляет Ping, GetStatus и GetInventory, сопоставляет ответы по
непрозрачным уникальным command_id и показывает результат. Agent отвечает `Pong`, `IDLE`
и текущим inventory, затем отправляет heartbeat с revision 0.

При разрыве Agent переподключается с backoff 1, 2, 4, 8, 16 и до 30 секунд; после
успешного handshake backoff сбрасывается. Ошибки несовместимых protocol/schema версий
считаются non-transient и не запускают reconnect storm. Cancellation останавливает loop.
Сетевой слой не управляет mining lifecycle; mining runtime в M1.2 отсутствует.

Loopback development example:

```sh
go run ./cmd/le0x-controller --listen 127.0.0.1:50051 --insecure-dev --init  # first start only
LE0X_DATA_DIR="$PWD/.le0x-data" go run ./cmd/le0x-agent --controller 127.0.0.1:50051 --insecure-dev
```

`--json` предназначен для локального режима и не комбинируется с `--controller`.
Controller поддерживает тот же флаг `--insecure-dev` для явного plaintext listener.
Узлы, сертификаты, CA, mTLS, pairing и persistent Controller state не реализованы.

## Структура

- `cmd/le0x-agent/` — запускаемый local bootstrap и CLI presentation.
- `cmd/le0x-controller/` — development-only Controller CLI skeleton.
- `internal/agentidentity/` — выбор data directory и атомарное хранение identity.
- `internal/agentnet/` — Agent gRPC client, handshake и reconnect backoff.
- `internal/runtime/supervisor/` — Linux child-process lifecycle and watchdog.
- `internal/minerruntime/` — generic miner adapter registry, telemetry polling and status normalization.
- `internal/miners/xmrig/` — XMRig-specific config, preflight and `/2/summary` adapter.
- `internal/packages/` — miner-agnostic verified local artifact store.
- `internal/controllernet/` — Controller gRPC stream handling and command correlation.
- `internal/wiremap/` — преобразование domain inventory в protobuf wire model.
- `internal/inventory/` — Linux discovery с подменяемыми источниками для тестов.
- `internal/identity/` — типобезопасные идентификаторы.
- `internal/model/` — доменные модели без исполняющей логики.
- `internal/farmerr/` — коды и структура ошибок.
- `internal/protocol/` — независимые типы ProtocolVersion и SchemaVersion.
- `internal/version/` — версия программного обеспечения.
- `proto/le0x/v1/` — v1-контракт, generated Go-файлы и тесты сериализации.
- `schemas/`, `docs/` — небольшие README с назначением каталогов.

## Решения M0

ID имеет вид `<type>_<32 lowercase hex digits>`. Каждый из двенадцати типов имеет
`New<Type>ID()`, `Parse<Type>ID(string)`, `String()` и `Validate()`.
Генерация использует 128 бит `crypto/rand` и не принимает hostname.
Совпадение hostname не влияет на идентичность хостов; вероятность случайной коллизии
пренебрежимо мала, но математическая гарантия уникальности без реестра не заявляется.
Новый ID следует создавать один раз и сохранять для дальнейшего использования.
Стабильность DeviceID между сканированиями должна обеспечиваться будущим хранением и
сопоставлением оборудования; GPU discovery в M1.1 отсутствует.

IDs — сравнимые структуры с закрытым значением. Доступны FarmID, ControllerID,
HostID, AgentID, NodaID, DeviceID, ProfileID, ExecutionID, ServiceID, WalletID,
PoolID и PackageID. Нулевое значение невалидно. Parse отклоняет другой префикс,
неверную длину и неканонический hex.

MarshalText и UnmarshalText обеспечивают text и JSON round trip, включая ключи map.
UnmarshalText заменяет значение только после успешного Parse; при ошибке прежнее
значение сохраняется. Таким образом, ID имеет семантику значения, но не строгую
неизменяемость: метод десериализации может присвоить ему новое значение.
Для JSON null отдельный запрет не реализован; Validate нужно вызывать при проверке
обязательных полей документа. Полная валидация доменных документов пока не определена.
YAML-библиотека не подключена. SQL Scanner/Valuer не реализованы.

Inventory описывает физическое оборудование: Host, CPU, GPUs и Memory.
Host.OS имеет тип OSInfo с ID, VersionID, PrettyName и Kernel.
Поля Cores и Threads — суммарные количества для хоста, память измеряется в байтах.
DeviceID GPU должен сохраняться между сканированиями.

DeviceConfig отдельно содержит HostID, CPUConfig и map[DeviceID]GPUConfig.
CPUConfig содержит Enabled, MiningThreads, MaxThreads, HugePages и MSR;
GPUConfig — Enabled, PowerLimitW и MaxPowerLimitW. Числовые настройки имеют тип
*uint32, HugePages и MSR — *bool. nil означает «не задано / auto / inherit»;
указатели на 0 и false задают явные значения. Enabled — обычный bool, по умолчанию false.
GPU, отсутствующий в map, не разрешён к использованию. Приоритеты auto/inherit
и проверка ограничений пока не реализованы.

WalletRef содержит WalletID, Name, Coin и только публичный payout Address.
Seed, private key и mnemonic не являются частью модели и не должны помещаться
в её строковые поля; распознавание секретов по содержимому строк не реализовано.
Pool содержит PoolID, Name, Coin и URL; Package — PackageID, Name, Version и SHA256.
MiningProfile ссылается на PackageID, WalletID и PoolID, а также содержит ProfileID,
Name и Coin. ServiceProfile использует ServiceID, Name, Kind, Coin и PackageID.

DesiredState содержит версию схемы, HostID, Revision, список DesiredMining
(ProfileID и DeviceIDs) и список ServiceID. Определений профилей и DeviceConfig в нём нет.
ExecutionPlan сохраняет domain references и optional CPUThreads, а M2 добавляет
resolved executable, argv, environment, working directory и restart policy.
ObservedState и ExecutionObservation сохраняют наблюдения по ExecutionID.

ProtocolVersion — версия взаимодействия компонентов; SchemaVersion — версия структуры
документов. Текущие значения равны 2 и 1 соответственно; ProtocolVersion повышен для
M3.5 resolved MiningEndpoint, чтобы M3 и M3.5 peers не смешивали несовместимые runtime
commands. Типы и дальнейшее изменение остаются независимыми.
Версия сборки `0.0.0-dev` хранится отдельно.

## M0.2 — protocol contracts

`proto/le0x/v1/agent_control.proto` использует protobuf package `le0x.v1` и Go package
`github.com/le0xdon/le0xfarm/proto/le0x/v1` (имя `le0xv1`).
`AgentControl.Connect` — двунаправленный streaming RPC: Agent отправляет AgentMessage,
Controller отвечает ControllerMessage. Сгенерированы только сообщения и gRPC stubs;
соединения не открываются, сервер и клиент runtime не реализованы.

Hello явно передаёт независимые protocol_version и schema_version как uint32.
Все IDs на wire — строки; protobuf не сериализует внутренние identity-типы.
Их проверка и преобразование относятся к будущей границе runtime.
command_id — непрозрачная строка корреляции, возвращаемая без изменения в CommandResult.
Heartbeat использует google.protobuf.Timestamp и uint64 observed_state_revision.
Ping/Pong содержат bytes nonce: Pong должен вернуть те же байты; это не аутентификация.

Команды включают Ping, GetStatus, GetInventory и M2 lifecycle operations для execution.
Результаты включают Pong, Status, минимальный Inventory, execution observations или
TypedError. Оболочки используют oneof. Proto3 допускает
незаполненный oneof и нулевые значения; обязательность полей, порядок hello и согласование
версий пока не реализованы. Номера полей нельзя переназначать; удалённые номера и имена
следует резервировать при будущих изменениях.

Status.agent_state — строка без преждевременного определения автомата состояний.
TypedError.code — строковый код из farmerr; details — map<string,string>.
Wire Inventory содержит только запрошенные физические факты, без DeviceConfig;
GPU UUID — пустая строка, если недоступен. Полных domain-конвертеров пока нет.

Генерация использует установленные инструменты: protoc 3.21.12, protoc-gen-go 1.36.12,
protoc-gen-go-grpc 1.6.2. Для воспроизводимого результата используйте эти версии.
Зависимости generated-кода закреплены в go.mod/go.sum: grpc 1.83.2 и protobuf 1.36.12.
Генераторы не скачиваются автоматически.

```sh
make proto
```

Оба generated-файла (`agent_control.pb.go`, `agent_control_grpc.pb.go`) должны храниться
в Git. `make check` повторяет генерацию во временном каталоге проекта и сравнивает
результат побайтно, включая обнаружение отсутствующих и лишних generated-файлов.
Проверка не перезаписывает их и удаляет временный каталог при завершении.
После изменения контракта выполните make proto и включите generated-файлы в ревью.

## M1.1 — Le0xAgent local bootstrap

Development target: Linux amd64, Ubuntu 22.04, 24.04 и 26.04 LTS.
Неизвестная версия Ubuntu не блокирует сбор inventory. Root и запись в /etc не нужны.

```sh
go run ./cmd/le0x-agent
go run ./cmd/le0x-agent --json
```

Команда выполняется один раз и завершается. Обычный вывод предназначен человеку;
--json выводит один JSON-объект без дополнительных строк: agent_id, host_id,
inventory (поля существующей domain model) и warnings. Ошибки выводятся в stderr,
при --json — как структурированный farmerr. Код выхода: 0 — успешный отчёт,
1 — ошибка bootstrap/вывода, 2 — неверные аргументы. --help не создаёт identity.

Порядок выбора data directory:

1. Непустой LE0X_DATA_DIR — явный каталог; относительный путь считается от cwd.
2. Абсолютный XDG_DATA_HOME — $XDG_DATA_HOME/le0xfarm/agent.
3. Иначе — $HOME/.local/share/le0xfarm/agent. Относительный XDG_DATA_HOME игнорируется.

Файл — identity.json, права 0600; каталог Agent — 0700. Новые промежуточные каталоги
создаются с 0700; уже существующие родительские каталоги не меняются.
Существующий конечный каталог с другими правами или symlink отклоняется.
Файл содержит schema_version: 1 (версия локального формата), host_id и agent_id.
Идентичность не зависит от hostname, IP, MAC или inventory. Изменение data directory
означает отдельную локальную identity; для сохранения идентичности нужен прежний файл.

При первом запуске полностью записанный временный файл синхронизируется через fsync,
после чего hard link атомарно публикует identity.json без перезаписи существующего имени.
Временное имя удаляется, каталог синхронизируется. Параллельные первые запуски используют
identity победившего процесса. Требуется локальная файловая система с поддержкой hard links
и directory fsync. При аварийном завершении до очистки может остаться временный файл;
он никогда не считается сохранённой identity и автоматически не восстанавливается.

Повреждение, неподдерживаемый формат, неверные ID, права или ошибка чтения приводят
к typed error и завершению; новый HostID вместо повреждённого не создаётся.
Symlink и нерегулярные identity-файлы отклоняются. Отсутствие итогового файла — единственный
повод для первой генерации. Восстановление повреждённой identity выполняется вручную
из доверенной копии; команда не удаляет и не исправляет существующий файл.

Источники inventory: os.Hostname, runtime.GOARCH, /etc/os-release (fallback
/usr/lib/os-release), /proc/sys/kernel/osrelease, /proc/cpuinfo и /proc/meminfo.
Никакие shell-команды не выполняются, os-release читается как данные без shell expansion.
CPU Threads — число логических процессоров, видимых в /proc/cpuinfo; Cores — уникальные
пары physical id/core id; Sockets — уникальные physical id. Это видимая ядру топология,
в VM — виртуальное оборудование. Неизвестные counts равны 0 и сопровождаются warnings.
MemTotal переводится из Linux kB (1024 байта) в байты. Недоступные факты дают частичный
отчёт и warnings. GPUs — пустой массив: GPU discovery ещё не выполнен, это не утверждение
об отсутствии видеокарт. Discovery не меняет DeviceConfig и права использования ресурсов.

Для локальной проверки внутри репозитория:

```sh
export LE0X_DATA_DIR="$PWD/.le0x-data"
go run ./cmd/le0x-agent
go run ./cmd/le0x-agent --json
```

Каталог .le0x-data/ исключён из Git. Makefile уже проверяет все новые пакеты;
дополнительных зависимостей для bootstrap не добавлено.

## Проверки

Требуются Go 1.26.0 или новее, make и указанные выше protobuf-генераторы в PATH.

```sh
make fmt
make test
make vet
make check
```

`make fmt` форматирует все пакеты проекта через `go fmt ./...`.
`make check` проверяет актуальность generated-кода, запускает тесты, vet и проверяет форматирование Go-файлов всех пакетов
из `go list ./...`, включая тесты, без изменения файлов.
Эквивалентные отдельные команды:

```sh
go fmt ./...
go test ./...
go vet ./...
```

Лицензия: Apache-2.0, см. LICENSE.

## M1.4 pairing foundation

Development pairing uses a one-time 256-bit enrollment token held only in Controller memory. Start an initialized Controller with `--pairing` (optionally `--pairing-ttl 15m`), then connect a new Agent with `--controller HOST:PORT --insecure-dev --pair TOKEN`. The token travels over plaintext development transport and is not production security. After pairing, Controller trust is stored in `paired_agents.json` and Agent trust in `controller.json`; reconnects do not require the token. Production pairing will use mTLS/bootstrap security in a later stage.

## M1.5 secure transport

The default Agent–Controller transport is persistent gRPC over TLS 1.3 with mutual TLS. The Farm CA and Controller certificate use Ed25519 and live under the Controller data directory in `pki/`. Existing M1.4 Controllers initialize this PKI once with `--init-pki`; a fresh `--init` creates identity and PKI together. PKI is never regenerated automatically. `--insecure-dev` remains an explicit plaintext development mode and is never used as a fallback.

Secure enrollment starts the Controller with `--pairing`. It prints a temporary token and the SHA-256 fingerprint of its current server certificate. The Agent reads the token from stdin and verifies the exact certificate fingerprint:

```sh
printf '%s\n' "$TOKEN" | le0x-agent --controller HOST:PORT --pair-stdin --tls-fingerprint SHA256:...
```

The Agent creates its Ed25519 key locally, sends only a signed CSR, persists the returned Agent certificate and Farm CA, closes the bootstrap stream, and reconnects with mutual TLS. Normal reconnect is simply `le0x-agent --controller HOST:PORT`. Controller certificates use the stable synthetic DNS name `<ControllerID>.controller.le0xfarm`; IP address, hostname and listen address do not define identity. Machine identities are also encoded in URI SANs and checked against protocol IDs and persistent trust.

Controller PKI permissions are `pki/` 0700, private keys 0600 and certificates 0644. Agent PKI uses the same directory and file permission policy. Farm CA validity is 10 years, Controller server certificate validity is one year, and Agent client certificate validity is 180 days. Automatic renewal is future work and should renew Agent certificates around 30 days before expiration.
