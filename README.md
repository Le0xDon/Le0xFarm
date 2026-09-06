# Le0xFarm

Le0xFarm — проект системы управления оборудованием, профилями майнинга и сервисами фермы.

Планируемые компоненты:
- **Le0xAgent** — локальный исполнитель на хосте: инвентаризация и применение заданного состояния.
- **Le0xController** — управление фермой и координация агентов.
- **Le0xNoda** — компонент для работы с нодами и связанными сервисами.
- **Le0xBrain** — будущая аналитика и автоматизация решений.

Сейчас реализуется только **M0 — FOUNDATION**. Есть типы IDs, минимальные модели,
структурированные ошибки и отдельные версии протокола и схемы. Runtime компонентов,
майнеры, Web UI, Telegram, dev mining и protobuf не реализованы.

## Структура

- `cmd/` — место будущих точек входа.
- `internal/identity/` — типобезопасные идентификаторы.
- `internal/model/` — доменные модели без исполняющей логики.
- `internal/farmerr/` — коды и структура ошибок.
- `internal/protocol/` — независимые типы ProtocolVersion и SchemaVersion.
- `internal/version/` — версия программного обеспечения.
- `proto/`, `schemas/`, `docs/` — небольшие README с назначением каталогов.

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

## Проверки

Требуются Go 1.26.0 или новее и make. Внешних Go-зависимостей нет.

```sh
make fmt
make test
make vet
make check
```

`make fmt` форматирует все пакеты проекта через `go fmt ./...`.
`make check` запускает тесты, vet и проверяет форматирование Go-файлов всех пакетов
из `go list ./...`, включая тесты, без изменения файлов.
Эквивалентные отдельные команды:

```sh
go fmt ./...
go test ./...
go vet ./...
```

Лицензия: Apache-2.0, см. LICENSE.
