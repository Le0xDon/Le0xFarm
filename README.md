# Le0xFarm

Le0xFarm — проект системы управления оборудованием, профилями майнинга и сервисами фермы.

Планируемые компоненты:
- **Le0xAgent** — локальный исполнитель на хосте: инвентаризация и применение заданного состояния.
- **Le0xController** — управление фермой и координация агентов.
- **Le0xNoda** — компонент для работы с нодами и связанными сервисами.
- **Le0xBrain** — будущая аналитика и автоматизация решений.

Сейчас реализован **M0.2 — protocol contracts** поверх M0.1 foundation: типы IDs,
минимальные модели, структурированные ошибки и protobuf/gRPC контракт Agent ↔ Controller.
Runtime компонентов, реальные соединения, mTLS, pairing, майнеры, Web UI, Telegram
и dev mining не реализованы.

## Структура

- `cmd/` — место будущих точек входа.
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
сопоставлением оборудования; сам механизм discovery в M0 отсутствует.

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
ExecutionPlan содержит только версию схемы, ExecutionID, HostID, ProfileID,
DeviceIDs и optional CPUThreads (*uint32). План пока не исполняется.
ObservedState и ExecutionObservation сохраняют наблюдения по ExecutionID.

ProtocolVersion — версия взаимодействия компонентов; SchemaVersion — версия структуры
документов. Оба начальных значения равны 1, но типы и дальнейшее изменение независимы.
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

Команды ограничены Ping, GetStatus и GetInventory. Результаты — Pong, Status,
минимальный Inventory или TypedError. Оболочки используют oneof. Proto3 допускает
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
