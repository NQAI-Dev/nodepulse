# Пишем легковесный мониторинг инфраструктуры на Go с прямым опросом Docker.sock и потреблением 5 МБ RAM

Когда инфраструктура разрастается от пары VPS до десятка серверов и десятков контейнеров, стандартный ответ индустрии — поставить связку **Prometheus + Node Exporter + cAdvisor + Grafana**.

Но что делать, если:
1. У вас небольшие ноды (1–2 ядра, 2–4 ГБ RAM), где один Prometheus с Grafana и cAdvisor сожрут 20–30% ресурсов сервера просто за факт своего существования?
2. Настройка дашбордов, алертов в Alertmanager и экспортеров занимает часы?
3. Хочется единый дашборд, который ставится одной командой за 5 секунд и сразу показывает не только железо, но и состояние всех Docker-контейнеров?

Мы разработали **NodePulse** — открытую распределенную платформу телеметрии и контроля инцидентов. В этой статье разберем, как устроен агент, почему мы отказались от внешних зависимостей и как собирать состояние Docker через Unix Domain Socket на чистом Go.

---

## Архитектура: ничего лишнего

NodePulse состоит из двух независимых компонентов:
1. **Edge Agent (`nodepulse-agent`)** — бинарник размером 8 МБ. Никаких внешних утилит, питонов или демонов. Потребление в рантайме: **~4–6 МБ RAM** и **<0.1% CPU**.
2. **Control Plane (`nodepulse-server`)** — центральный сервер со встроенным веб-интерфейсом, скользящим окном истории (TimeSeries in-memory), персистентным SQLite для инцидентов и диспетчером алертов в Telegram и Webhook.

---

## Как агент заглядывает в Docker без Docker CLI

Стандартная ошибка при написании легковесных агентов — вызывать `exec.Command("docker", "ps")`. Это порождает лишние форки процессов каждые N секунд, жрет CPU и требует установленного CLI.

Вместо этого мы используем нативный стандартный пакет `net` в Go и общаемся с `/var/run/docker.sock` напрямую по HTTP через Unix сокет:

```go
package collector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
)

type DockerContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

func CollectDockerServices() []ServiceStatus {
	sock := "/var/run/docker.sock"
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
		Timeout: 2 * time.Second,
	}

	resp, err := client.Get("http://localhost/containers/json?all=1")
	if err != nil {
		return nil // Docker не запущен или нет прав на сокет
	}
	defer resp.Body.Close()

	var containers []DockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil
	}

	var services []ServiceStatus
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		services = append(services, ServiceStatus{
			Name:   name,
			Type:   "docker",
			Active: c.State == "running",
			Status: c.Status,
		})
	}
	return services
}
```

Этот вызов отрабатывает за доли миллисекунды и возвращает исчерпывающее состояние контейнеров на ноде.

---

## Сбор системных метрик без сторонних библиотек

Вместо подключения тяжелых библиотек вроде `gopsutil`, агент читает виртуальные файловые системы Linux:
- `/proc/meminfo` — парсинг `MemTotal` и `MemAvailable` дает точный объем занятой памяти с учетом кэшей ядра.
- Системный вызов `syscall.Sysinfo` — мгновенное получение `LoadAverage (1m, 5m, 15m)`.
- Системный вызов `syscall.Statfs` — заполненность дисковых разделов.

Никаких CGO-зависимостей. Бинарник собирается со `CGO_ENABLED=0` и запускается на любом дистрибутиве Linux от Alpine до Debian и CentOS.

---

## Установка за 5 секунд

Чтобы подключить сервер к мониторингу, не нужно править конфиги. В личном кабинете дается готовая строка:

```bash
curl -sSL https://pulse.nqai.es-cloud.ru/install.sh?token=YOUR_TOKEN | sh
```

Скрипт сам определяет архитектуру, скачивает агент в `/usr/local/bin`, создает systemd-сервис и стартует его. Сервер мгновенно появляется в списке активного флота.

---

## Что в итоге получилось

- **Живой демо-инстанс:** [pulse.nqai.es-cloud.ru](https://pulse.nqai.es-cloud.ru)
- **Исходный код на GitHub:** [github.com/NQAI-Dev/nodepulse](https://github.com/NQAI-Dev/nodepulse)
- Встроенный Prometheus Exporter на `/metrics`.
- Алерты о сбоях и упавших контейнерах в Telegram в реальном времени.

Будем рады звездочкам на GitHub, фидбеку и issue!
