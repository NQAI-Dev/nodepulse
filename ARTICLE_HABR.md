# Как мы написали мониторинг серверов на Go с потреблением 4 МБ RAM, прямым парсингом /proc и сокетом Docker

Когда инфраструктура вырастает от одного VPS до десятка серверов и десятков микросервисов, дежурный ответ любого девопса — поставить классический стек: **Prometheus + Node Exporter + cAdvisor + Grafana + Alertmanager**.

Это надежный индустриальный стандарт. Но у него есть обратная сторона, с которой сталкивался каждый владелец скромного парка серверов:

1. **Оверхед на маленьких нодах.** Если у вас VPS на 1–2 ядра и 1–2 ГБ памяти (а таких в пет-проектах и стартапах большинство), один Node Exporter + cAdvisor съедают 200–400 МБ RAM. Добавьте сюда сам Prometheus с TSDB на центральной ноде — и треть ресурсов уходит на обслуживание самого мониторинга.
2. **Ад конфигураций.** Чтобы связать дашборд Grafana, настроить scrape configs, прописать правила алертинга в Alertmanager и пробросить экспортеры через firewall/mTLS, уходит несколько часов вдумчивого ковыряния YAML.
3. **Отсутствие авто-восстановления (Auto-healing).** Традиционный мониторинг пассивен: он кричит в чат «сервис упал!», будит инженера в 3 часа ночи, хотя в 90% случаев решение тривиально — сделать `docker restart` или `systemctl restart`.

Мы решили написать альтернативу — **NodePulse**. Это распределенная система мониторинга на чистом Go, где агент весит 7 МБ, потребляет в рантайме **4–6 МБ оперативной памяти**, ставится за 5 секунд одной строкой и умеет автоматически перезапускать упавшие сервисы без внешних демонов.

В этой статье разберем инженерные решения: как читать метрики ядра напрямую из виртуальной ФС без сторонних либ, как опрашивать Docker API через Unix-сокет на чистом `net/http` и как устроен контур remediation.

---

## Архитектурный каркас: Push против Pull

Prometheus использует модель **Pull**: сервер обходит ноды по расписанию и опрашивает открытые HTTP-порты экспортеров. 
Для распределенной инфраструктуры с серверами за NAT, динамическими IP или строгими файрволами это создает боль — нужны reverse proxy, туннели или VPN.

В NodePulse мы выбрали архитектуру **Push с управляющей связью (Heartbeat & Command Dispatch)**:

```
+-------------------------------------------------------------+
|                      Target Host (Node)                     |
|                                                             |
|  +------------------+   +----------------+   +-----------+  |
|  |   /proc & sys    |   |  Docker Socket |   |  systemd  |  |
|  +--------+---------+   +-------+--------+   +-----+-----+  |
|           |                     |                  |        |
|           +----------> [ nodepulse-agent ] <-------+        |
|                              |      ^                       |
+------------------------------|------|-----------------------+
                1. Push State  |      | 2. Remediation Cmds   |
                (Heartbeat)    v      | (Auto-heal)           |
+-------------------------------------------------------------+
|               Control Plane (nodepulse-server)              |
|                                                             |
|  +---------------------+  +-----------------+  +----------+ |
|  | In-Memory Ring TSDB |  | Persistent DB   |  | Alerters | |
|  | (Metrics sliding)   |  | (SQLite Fleet)  |  | (TG/Web) | |
|  +---------------------+  +-----------------+  +----------+ |
|                            |                                |
|                 [ Web UI & Status Page ]                    |
+-------------------------------------------------------------+
```

Каждые 5 секунд агент отправляет компактный сжатый JSON-пейлоад в сторону сервера через защищенный HTTPS-эндпоинт `/api/v1/ingest`. В теле ответа сервер может вернуть агенту команды на исполнение (например, перезапуск сервиса при зафиксированном инциденте).

---

## Детали реализации агента

Главное требование к агенту — абсолютная автономность. Никаких рантаймов Python, никаких утилит `top`, `free`, `iostat` или `docker` в системе. Только один статический бинарник без CGO (`CGO_ENABLED=0`).

### 1. Сбор системных метрик без `gopsutil`

Популярная библиотека `gopsutil` тянет за собой сотни килобайт вспомогательного кода и часто парсит лишнее. В Linux ядро отдает всю правду о системе через псевдо-файловые системы `/proc` и системные вызовы ядра.

#### Память (`/proc/meminfo`):
Ошибочно вычислять свободную память как `MemFree`. В Linux свободная память активно используется под дисковые кэши (`Buffers` и `Cached`), которые ядро освобождает по первому требованию. Реальное доступное пространство — это `MemAvailable` (появился в ядре с версии 3.14).

```go
func parseMemory() (total, available uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = val * 1024
		case "MemAvailable:":
			available = val * 1024
		}
	}
	return total, available, scanner.Err()
}
```

#### Загрузка процессора (`syscall.Sysinfo`):
Вместо чтения и вычисления дельт из `/proc/stat` для базового мониторинга достаточно получать Load Average и аптайм напрямую через системный вызов ядра:

```go
var si syscall.Sysinfo_t
if err := syscall.Sysinfo(&si); err == nil {
	// Значения Loads в Linux нормализованы со сдвигом 16 бит (фиксированная точка)
	load1 := float64(si.Loads[0]) / 65536.0
	load5 := float64(si.Loads[1]) / 65536.0
	load15 := float64(si.Loads[2]) / 65536.0
	uptime := si.Uptime
}
```

#### Дисковое пространство (`syscall.Statfs`):
Получение свободных блоков ФС без форка команды `df -h`:

```go
var fs syscall.Statfs_t
if err := syscall.Statfs("/", &fs); err == nil {
	totalBytes := fs.Blocks * uint64(fs.Bsize)
	// Важно использовать Bavail (блоки для непривилегированных пользователей), а не Bfree
	freeBytes := fs.Bavail * uint64(fs.Bsize)
	usedPercent := float64(totalBytes - freeBytes) / float64(totalBytes) * 100.0
}
```

---

### 2. Прямой опрос Docker через сокет без Docker CLI и SDK

Официальный Docker Go SDK (`github.com/docker/docker/client`) тянет за собой огромный транзитивный граф зависимостей. 

Демон Docker общается через стандартный REST API по Unix Domain сокету `/var/run/docker.sock`. Стандартная библиотека Go (`net/http`) из коробки умеет работать с любым `net.Conn`, включая Unix Domain Socket.

Вот как выглядит получение списка контейнеров за 20 строк:

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

type ContainerInfo struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

func GetDockerContainers() ([]ContainerInfo, error) {
	socketPath := "/var/run/docker.sock"
	
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 2 * time.Second,
	}

	resp, err := client.Get("http://localhost/containers/json?all=1")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var containers []ContainerInfo
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	return containers, nil
}
```

**Плюсы такого подхода:**
- 0 внешних зависимостей.
- Время выполнения запроса — менее 1 миллисекунды.
- Работает везде, где запущен Docker, независимо от наличия утилиты `docker` в `$PATH`.

---

### 3. Auto-Healing: когда мониторинг чинит сам

Обычный сценарий при OOM-killer или падении процесса:
1. Контейнер упал.
2. Prometheus через 30 секунд зафиксировал отсутствие метрики.
3. Alertmanager через 1–2 минуты отправил алерт.
4. Человек увидел алерт через 10 минут, открыл ноутбук, зашел по SSH, набрал `docker start my-service`.

В NodePulse заложен контур автоматического восстановления:

1. Сервер видит, что критический контейнер или systemd-юнит перешел в статус `exited` / `failed`.
2. Сервер фиксирует инцидент, генерирует алерт и отправляет в ответном heartbeat агенту команду `RESTART_DOCKER_CONTAINER` или `RESTART_SYSTEMD_UNIT`.
3. Агент отправляет `POST http://localhost/containers/{id}/restart` в локальный docker.sock:

```go
func RestartContainer(containerID string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", "/var/run/docker.sock")
			},
		},
		Timeout: 10 * time.Second,
	}

	req, _ := http.NewRequest("POST", "http://localhost/containers/"+containerID+"/restart", nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
```

Сервис поднимается за 1–2 секунды после падения. В чат Telegram уходит отчет: *«Сервис web-api упал на ноде esc-node-ru, автоматически перезапущен (Status: Running)»*.

---

## Архитектура сервера и хранилища

Сервер NodePulse решает две задачи:
1. **Горячая телеметрия (Hot Path):** Отображение графиков CPU, RAM, диска и сети за последние 30–60 минут в реальном времени.
2. **Холодная история (Cold Path):** Инциденты, журнал доступности (SLA), авторизация нод и биллинг.

### Кольцевой буфер (Ring Buffer) в памяти для метрик
Складывать каждую точку 5-секундного тика в реляционную БД на диск — верный способ убить IOPS дешевого NVMe. Для каждого хоста в памяти сервера выделен кольцевой буфер фиксированного размера (например, 720 точек = 1 час истории):

```go
type RingBuffer struct {
	mu      sync.RWMutex
	points  []DataPoint
	maxSize int
	head    int
}

func (r *RingBuffer) Push(p DataPoint) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.points) < r.maxSize {
		r.points = append(r.points, p)
	} else {
		r.points[r.head] = p
		r.head = (r.head + 1) % r.maxSize
	}
}
```

Чтение истории для фронтенда занимает **микросекунды** без единого дискового чтения. А критические события (регистрация ноды, падение доступности, инциденты) пишутся в SQLite с включенным WAL-режимом (`PRAGMA journal_mode=WAL;`).

---

## Сравнение потребления ресурсов: NodePulse vs Prometheus Stack

Мы замерили потребление памяти на одной и той же ноде (Debian 12, 2 vCPU, 2 GB RAM, 6 Docker-контейнеров):

| Компонент | RAM (RSS) | CPU в простое | Зависимости |
| :--- | :--- | :--- | :--- |
| **Node Exporter + cAdvisor** | ~180–240 МБ | 1.5–3.0% | libc, Docker runtime |
| **Prometheus + Alertmanager** | ~400–800 МБ | 2.0–5.0% | Сложная TSDB, FS |
| **NodePulse Agent** | **4.2 МБ** | **< 0.05%** | **0 зависимостей (Static)** |
| **NodePulse Server (включая Web UI)** | **18–25 МБ** | **< 0.2%** | **Один бинарник + SQLite** |

---

## Публичная статус-страница и интеграции

Для любого сервиса важна прозрачность перед пользователями. В NodePulse из коробки встроена публичная страница статуса (`/status.html`), которая берет данные напрямую из состояния флота:
- Доступность компонентов (Operational, Degraded, Major Outage).
- Текущие активные инциденты и история сбоев за 90 дней.
- Готовый `/api/v1/public/status` для подключения внешних систем или сторонних виджетов.

---

## Как попробовать

Платформа полностью открыта под лицензией MIT.

1. **Репозиторий с исходным кодом:** [github.com/NQAI-Dev/nodepulse](https://github.com/NQAI-Dev/nodepulse)
2. **Публичный дашборд:** [pulse.nqai.es-cloud.ru](https://pulse.nqai.es-cloud.ru)
3. **Установка агента на любую ноду за 5 секунд:**
```bash
curl -sSL https://pulse.nqai.es-cloud.ru/install.sh?token=ВАШ_ТОКЕН | sh
```

Будем рады конструктивной критике, пулл-реквестам и обсуждению архитектурных решений в комментариях!
