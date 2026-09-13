# ⚡ NodePulse

> **Next-generation distributed infrastructure monitoring, Docker telemetry & incident control plane.**

![NodePulse Live](https://img.shields.io/badge/status-live-brightgreen)
![Go](https://img.shields.io/badge/go-1.24-blue)
![Docker](https://img.shields.io/badge/docker-monitored-2496ED)
![License](https://img.shields.io/badge/license-MIT-green)

NodePulse — открытая облачная и self-hosted платформа для мониторинга серверов, контейнеров и инцидентов в реальном времени.

**Официальный инстанс:** [https://pulse.nqai.es-cloud.ru](https://pulse.nqai.es-cloud.ru)

---

## 🚀 Быстрый старт (Установка агента за 5 секунд)

Подключите любой Linux-сервер к мониторингу одной командой:

```bash
curl -sSL https://pulse.nqai.es-cloud.ru/install.sh | sh
```

Или с вашим персональным токеном из личного кабинета:

```bash
curl -sSL https://pulse.nqai.es-cloud.ru/install.sh?token=YOUR_API_TOKEN | sh
```

Скрипт автоматически:
1. Загружает бинарник `nodepulse-agent` под архитектуру машины.
2. Подключается к сокету `/var/run/docker.sock` для телеметрии контейнеров.
3. Регистрирует и запускает демон systemd `nodepulse-agent.service`.

---

## ✨ Ключевые возможности

- **Zero-Dependency Edge Agent**: Минималистичный бинарник на Go, потребляющий <10 МБ RAM и <0.1% CPU.
- **Прямая интеграция с Docker**: Автоматическое обнаружение контейнеров, их статусов, uptime и падений через Unix Domain Socket без внешних CLI.
- **Встроенный Multi-Tenancy**: Личный кабинет, регистрация, выдача токенов и строгая изоляция нод между пользователями.
- **Incident & Alert Engine**: Диспетчер алертов в Telegram и по Webhook при критической нагрузке (RAM > 92%, OOM, падение контейнеров) с возможностью резолва прямо из UI.
- **Prometheus Exporter**: Нативный эндпоинт `/metrics` для сопряжения с корпоративными системами мониторинга.
- **Mobile-First UI**: Быстрый, реактивный интерфейс без внешних фреймворков со встроенными SVG sparkline-графиками нагрузки.

---

## 🛠 Self-Hosted развертывание

Разверните собственный центральный сервер Control Plane:

```bash
git clone https://github.com/NQAI-Dev/nodepulse.git
cd nodepulse
docker compose up -d
```

Сервер будет доступен на порту `8080`.

---

## 🔐 Оператор: онбординг пользователей через Telegram

NodePulse использует одноразовые invite-токены для привязки Telegram-чата пользователя к его аккаунту (или для авто-создания нового). Поток закрывает базовый сценарий «оператор знает пользователя лично и хочет выдать ему доступ», не требуя регистрации через Telegram Login Widget или email/пароль.

### 1. Выпустить инвайт (mint)

Мастер-токен нужен только оператору. Используйте скрипт-обёртку:

```bash
./scripts/mint-invite.sh --target-uid 7 --expires-hours 1 --username Distemi
# stdout: https://t.me/nodepulse_mon_bot?start=np_inv_c4d8c254a53a16374578f5600a130869
# stderr: [mint-invite] token=np_inv_c4d8c254a53a16374578f5600a130869 expires=2026-09-13 22:55:09 UTC
```

Скрипт сам найдёт мастер-токен (флаг `--master-token`, переменная `NODEPULSE_MASTER_TOKEN`, или `/etc/nodepulse/nodepulse-server.env`), сформирует валидный JSON и распарсит ответ. Без аргументов — пустой инвайт с бессрочным сроком и auto-create пользователя при redeem. Полный список флагов: `--help`. `--dry-run` печатает запрос без отправки.

Прямой API (если нужно руками, без скрипта):

```bash
curl -sS -X POST https://pulse.nqai.es-cloud.ru/api/v1/invites \
  -H "Authorization: Bearer $NODEPULSE_MASTER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"target_user_id":7,"default_username":"Distemi","expires_in_hours":1}'
# → {"token":"np_inv_...","deep_link":"https://t.me/nodepulse_mon_bot?start=np_inv_...","expires_at":1789340109}
```

### 2. Отправить deep-link пользователю

Скопируйте `deep_link` и отправьте пользователю в Telegram. По нажатию бот `@nodepulse_mon_bot` откроет диалог и попросит пользователя нажать `/start`. Бот распознает токен и перенаправит запрос на сервер.

### 3. Активация (redeem)

Бот вызывает `POST /api/v1/invites/redeem` с `{"token":"np_inv_...","chat_id":<telegram_chat_id>}`. Сервер атомарно (single `UPDATE ... WHERE token=? AND redeemed_at=0`) помечает инвайт использованным и возвращает:

- `200` + `{ok:true, redeemed_at:..., target_user_id:7}` — чат привязан к существующему пользователю #7.
- `200` + `{ok:true, ..., target_user_id:0}` — инвайт с `auto_create_user=true`, новый пользователь создан.
- `409` — инвайт уже использован (другой чат). Атомарность гарантирует, что только один redeem успешен.
- `404` — токен не найден.
- `410` — истёк (если был задан `expires_in_hours`).

### 4. Аудит и revoke

Список активных и погашенных инвайтов (токены маскированы до `np_inv_<12chars>…`):

```bash
curl -sS "https://pulse.nqai.es-cloud.ru/api/v1/invites?limit=20" \
  -H "Authorization: Bearer $NODEPULSE_MASTER_TOKEN"
# → {"invites":[{"token":"np_inv_c4d8c254…","target_user_id":7,"auto_create_user":false,
#                 "created_at":"...","expires_at":"...","redeemed_at":"...",
#                 "redeemed_by_chat_id":111222333}, ...]}
```

Мастер-токен хранится в `/etc/nodepulse/nodepulse-server.env` (mode 0600, root:root) — единственный путь восстановления без пересборки БД.

---

## 📜 Лицензия

MIT License © 2026 [NQAI](https://github.com/NQAI-Dev)
