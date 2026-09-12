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

## 📜 Лицензия

MIT License © 2026 [NQAI](https://github.com/NQAI-Dev)
