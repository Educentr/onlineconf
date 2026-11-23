# Релиз OnlineConf: admin и updater

Как собрать и выпустить образы `onlineconf-admin` и `onlineconf-updater`. Процесс
проверен 21.11.2026 на первом релизе из нового места (`v3.6.6` и
`onlineconf-updater/v3.6.5`). Документ самодостаточный — его можно отдать агенту.

Живёт рядом с кодом, который описывает (TOOLS-126). До этого лежал в репозитории
infra как `onlineconf-release.md` — та копия больше не актуальна: имя Go-модуля
updater с 22.11.2026 другое.

## Где что

| Что | Где |
|---|---|
| Код | [github.com/Educentr/onlineconf](https://github.com/Educentr/onlineconf), ветка `master` (наш форк `onlineconf/onlineconf`; до 21.11.2026 — `Nikolo/onlineconf`) |
| CI | `.github/workflows/admin.yml`, `.github/workflows/updater.yml` — по одному на компонент |
| Образы | `ghcr.io/educentr/onlineconf-admin`, `ghcr.io/educentr/onlineconf-updater` — **публичные**, тянутся без логина |
| Вход CI в ghcr | `GITHUB_TOKEN` с `permissions: packages: write` — личных токенов нет |
| Go-модуль updater | `github.com/Educentr/onlineconf/onlineconf-updater/v3` (переименован 22.11.2026, см. «Ограничения») |
| Go-модуль admin | `github.com/onlineconf/onlineconf/admin/go` — имя upstream'а, намеренно оставлено (см. «Ограничения») |

| Компонент | Каталог | Тег релиза | Тег образа |
|---|---|---|---|
| admin | `admin/` | `vX.Y.Z` | `:vX.Y.Z` |
| updater | `onlineconf-updater/` | `onlineconf-updater/vX.Y.Z` | `:vX.Y.Z` (префикс срезается) |

Префикс тега updater — не украшение: Go-модуль лежит в подкаталоге
`onlineconf-updater/`, и Go ищет его версии именно по тегам
`onlineconf-updater/vX.Y.Z`. Тег updater одновременно выпускает и образ, и
версию Go-модуля.

## Что делает CI

| Событие | Результат |
|---|---|
| PR, задевающий каталог компонента или его workflow | только сборка, в ghcr ничего не уходит — проверка, что Dockerfile собирается |
| push в `master`, задевающий каталог компонента или его workflow | образ `:<sha7>` коммита |
| тег релиза | `:vX.Y.Z` и `:latest` на образ `:<sha7>` коммита тега; если такого образа нет (коммит не трогал компонент, и master его не собирал) — сначала собирает его |
| Actions → «Run workflow» на ветке | `:<sha7>` выбранного коммита |
| Actions → «Run workflow» на теге | то же, что push тега |

Если образ `:<sha7>` уже есть, он не пересобирается — релиз только навешивает
теги на готовый образ (в логе: `… already exists, not rebuilding`). Поэтому
`:vX.Y.Z`, `:latest` и `:<sha7>` релиза — один и тот же digest.

`<sha7>` — первые 7 символов полного SHA коммита (`${sha::7}`), а не
`git rev-parse --short`: длина второго зависит от истории.

## Как выпустить релиз

Нужны: права записи в `Educentr/onlineconf`, `git`, `gh` (залогинен), `curl`,
`jq`. Пример — admin `v3.6.7`; для updater подставить тег
`onlineconf-updater/vX.Y.Z` и имя образа `onlineconf-updater`.

**1. Убедиться, что `master` зелёный и образ коммита есть.**

```bash
gh run list --repo Educentr/onlineconf --branch master --limit 5
```

Последние запуски `admin image` / `updater image` — `success`. Если коммит не
трогал компонент, запуска для него не будет — это нормально, релиз соберёт образ
сам.

**2. Выбрать номер.** Последние выпущенные:

```bash
git fetch --tags origin
git tag -l 'v*' --sort=-v:refname | head -3                      # admin
git tag -l 'onlineconf-updater/v*' --sort=-v:refname | head -3   # updater
```

Номера admin и updater независимы (на 21.11.2026: admin `v3.6.6`, updater
`v3.6.5`). Что вошло в релиз с прошлого раза:

```bash
git log --oneline v3.6.6..origin/master -- admin/
git log --oneline onlineconf-updater/v3.6.5..origin/master -- onlineconf-updater/
```

**3. Поставить аннотированный тег на коммит `master` и запушить.**

```bash
git tag -a v3.6.7 -m "onlineconf-admin v3.6.7: <что изменилось>" origin/master
git push origin v3.6.7
```

**4. Дождаться запуска.**

```bash
gh run list --repo Educentr/onlineconf --workflow admin.yml --limit 3
gh run watch --repo Educentr/onlineconf <id запуска>
```

**5. Проверить без авторизации — так, как тянут потребители.**

```bash
img=educentr/onlineconf-admin; ver=v3.6.7
t=$(curl -s "https://ghcr.io/token?scope=repository:$img:pull" | jq -r .token)
curl -s -H "Authorization: Bearer $t" "https://ghcr.io/v2/$img/tags/list" | jq -c .tags
for tag in "$ver" latest; do
  curl -sI -H "Authorization: Bearer $t" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json' \
    "https://ghcr.io/v2/$img/manifests/$tag" | grep -i docker-content-digest
done
```

`:vX.Y.Z` и `:latest` должны дать одинаковый digest. Для updater ещё проверить,
что Go увидел версию (имя модуля в URL — с `!` перед бывшей заглавной буквой;
одинарные кавычки обязательны, иначе bash примет `!educentr` за ссылку на историю):

```bash
curl -s 'https://proxy.golang.org/github.com/!educentr/onlineconf/onlineconf-updater/v3/@v/v3.7.0.info'
```

Ответ — JSON с `"Ref":"refs/tags/onlineconf-updater/v3.7.0"`. Если proxy отвечает
404, а тег запушен — модуль этой версии ещё никто не запрашивал; первый запрос
наполняет кэш proxy:

```bash
GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org \
  go install github.com/Educentr/onlineconf/onlineconf-updater/v3@v3.7.0
```

После этого `.info` отдаёт версию.

**6. (Необязательно) GitHub Release** — раньше их заводили руками
(`v3.6.4`, `v3.6.5`); CI их не создаёт:

```bash
gh release create v3.6.7 --repo Educentr/onlineconf --title v3.6.7 --notes "…"
```

**7. Обновить закреплённые версии образов в compose-файлах** репозитория —
`admin/docker-compose.yml`, `admin/docker-compose-nginx.yml`,
`onlineconf-updater/docker-compose.yml` ссылаются на `ghcr.io/educentr/…` с
конкретной версией, а не на `:latest`, чтобы локальный стенд был
воспроизводим. Это обычный коммит в `master` после релиза, не часть релиза.

## Чего не делать

- **Не переставлять и не удалять выпущенный тег.** Go proxy запоминает версию
  модуля навсегда, а потребители могли уже стянуть образ. Ошибся — выпусти
  следующую версию.
- **Не выпускать тег на старый коммит без нужды:** `:latest` перейдёт на этот
  старый образ. `:latest` — это «последний запушенный тег», а не «максимальная
  версия».
- **Не публиковать руками** (`docker push` с ноутбука) — только CI, иначе образ
  и коммит расходятся.
- **Не использовать `ghcr.io/nikolo/onlineconf-*`** для нового: это образы до
  21.11.2026, они числятся за личным аккаунтом, к репозиторию не привязаны и
  новых тегов не получат.

## Ограничения и ловушки

- **Новый пакет в ghcr рождается приватным.** Если появится третий компонент
  (или пакет пересоздадут), первый push сделает его private, и потребители
  получат 401. Сделать публичным может только админ организации в интерфейсе:
  `https://github.com/orgs/Educentr/packages/container/<имя>/settings` → Danger
  Zone → Change visibility. Через API видимость не меняется. Отличить
  «приватный» от «нет такого» снаружи: анонимный запрос токена
  `https://ghcr.io/token?scope=repository:educentr/<имя>:pull` даёт **401** на
  приватный и **403** на несуществующий.
- **Правка файлов workflow через https требует у токена scope `workflow`.** У
  `gh` по умолчанию его нет — push отвергается с «refusing to allow an OAuth App
  to create or update workflow». Либо пушить по SSH, либо
  `gh auth refresh -s workflow`.
- **Имя Go-модуля updater сменилось на `github.com/Educentr/onlineconf/…`**
  (22.11.2026, TOOLS-126). Версии `≤ onlineconf-updater/v3.6.5` остались в
  proxy.golang.org под старым именем `github.com/Nikolo/onlineconf/…` и никуда
  не денутся, но новых под ним не будет. Для потребителя это ломающее изменение:
  старый путь в его `go.mod` / `go install` не подхватит новые версии сам, его
  надо переписать (`Educentr/go-project-starter` — TOOLS-127). Смена пути —
  причина, по которой следующая версия updater берёт minor, а не patch (`v3.7.0`).
- **Модуль admin остался под именем upstream'а** —
  `github.com/onlineconf/onlineconf/admin/go`. Решение TOOLS-126: admin — это
  приложение, его собирают из каталога `admin/`, как Go-модуль его не импортирует
  никто (ни внутри компании, ни снаружи), а тегов `admin/go/vX.Y.Z` под него
  нет вовсе (модуль лежит в `admin/go/`) — релиз admin ходит под тегом `vX.Y.Z`,
  который Go для модуля в подкаталоге не видит. Переименование дало бы только
  расхождение с upstream'ом и лишний шум в merge'ах из него. Если admin
  когда-нибудь понадобится как библиотека — переименовать вместе с переходом на
  теги `admin/go/vX.Y.Z`.
- **Базовые образы в Dockerfile без версий** (`FROM golang`, `FROM debian`,
  `node:20`): сборка одного и того же коммита в разные дни даёт разные образы.
  Именно поэтому релиз не пересобирает, а переиспользует `:<sha7>`.

## Кто потребляет

- `Educentr/go-project-starter`: шаблон compose — образ
  `ghcr.io/nikolo/onlineconf-updater:5ee228a`; Makefile — `go install
  github.com/Nikolo/onlineconf/onlineconf-updater/v3@v3.6.4`; dev-стенд admin
  собирается из исходников **upstream** `onlineconf/onlineconf` (submodule), не
  из форка. Перевод на `ghcr.io/educentr/…` и на новое имя модуля — **TOOLS-127**,
  за его мейнтейнерами; этой задаче нужен выпущенный `onlineconf-updater/v3.7.0`.
- Внутренний стенд OnlineConf (репозиторий infra) образы из
  ghcr **не** берёт: собирает admin из чекаута ветки `nginx-conf-auth`. Ссылки
  на старое место в infra (адрес репозитория, `ghcr.io/nikolo/…:03daa51` в
  compose оркестратора) переводит **INFRA-3**.
- compose-файлы этого репозитория — локальный стенд и демо; версии в них
  закреплены (`ghcr.io/educentr/onlineconf-admin:v3.6.6`,
  `ghcr.io/educentr/onlineconf-updater:v3.7.0`) и поднимаются шагом 7 релиза.
