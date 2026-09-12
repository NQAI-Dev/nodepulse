# Грабли, cgroups и прямой сокет Docker: что мы поняли, пока писали свой демон мониторинга на Go

Когда разворачиваешь Prometheus, Node Exporter, cAdvisor и Grafana на серверах с 1–2 ГБ RAM, быстро понимаешь: мониторинг потребляет больше, чем полезная нагрузка. Для небольших VDS или IoT-нод держать связку, съедающую 300–600 МБ памяти просто за факт своего существования, расточительно.

Мы решили написать компактный агент сбора метрик на чистом Go с минимальным футпринтом (~4 МБ RAM) и нулевыми внешними зависимостями. В процессе разработки мы наступили на все классические грабли низкоуровневой работы с Linux: от ложных метрик памяти до особенностей Unix Domain сокетов и поведения `syscall` внутри контейнеров.

Ниже — разбор практических граблей, код и выводы, которые сэкономят время тем, кто пишет системные утилиты на Go под Linux.

---

## Грабли 1. `MemFree` — это не свободная память, а `MemAvailable` не всегда доступен

Первое искушение при парсинге `/proc/meminfo` — взять поле `MemFree:`:

```
MemTotal:        2015948 kB
MemFree:           82340 kB
MemAvailable:    1420112 kB
Buffers:           34120 kB
Cached:          1350412 kB
```

Если ориентироваться на `MemFree`, система с 2 ГБ памяти покажет, что свободно всего 80 МБ, хотя на самом деле доступно 1.4 ГБ. 

В Linux неиспользуемая память — потерянная память. Ядро агрессивно задействует RAM под дисковый кэш (page cache) и буферы ввода-вывода (`Cached` + `Buffers`). При нехватке памяти под процессы ядро сбрасывает чистые страницы кэша мгновенно, без задержек.

### Как правильно:
Начиная с ядра 3.14 (2014 год) в `/proc/meminfo` появилось поле `MemAvailable:`. Ядро само оценивает, сколько страниц памяти можно выделить без ухода в swap:

```go
func ParseMemory() (total, available uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			total = parseMemKb(line) * 1024
		} else if strings.HasPrefix(line, "MemAvailable:") {
			available = parseMemKb(line) * 1024
		}
	}
	// Fallback для старых ядер (< 3.14) или специфичных OpenVZ контейнеров
	if available == 0 && total > 0 {
		// Грубая оценка: Free + Buffers + Cached
		// Но с оговоркой: часть Cached может быть грязной (dirty) или в shmem
	}
	return total, available, scanner.Err()
}
```

**Подводный камень:** если ваш агент запускается внутри LXC/OpenVZ старых версий или Docker-контейнера без проброса cgroups, `/proc/meminfo` показывает память **хоста**, а не лимит контейнера. Для контейнеров нужно дополнительно проверять `/sys/fs/cgroup/memory/memory.limit_in_bytes` (cgroups v1) или `/sys/fs/cgroup/memory.max` (cgroups v2).

---

## Грабли 2. `syscall.Sysinfo` быстрый, но слепой

Чтобы не парсить `/proc/loadavg` и `/proc/uptime`, в Go часто используют системный вызов `syscall.Sysinfo`:

```go
var si syscall.Sysinfo_t
if err := syscall.Sysinfo(&si); err == nil {
	// Внимание: si.Loads хранит значения с фиксированной точкой (сдвиг 16 бит)
	load1 := float64(si.Loads[0]) / 65536.0
	load5 := float64(si.Loads[1]) / 65536.0
	load15 := float64(si.Loads[2]) / 65536.0
	uptime := time.Duration(si.Uptime) * time.Second
}
```

Этот вызов исполняется за микросекунды и не требует открытия файлов.

### В чем подвох:
1. **Фиксированная точка ядра.** Значения `si.Loads` — это целые числа, где реальный float умножен на `(1 << 16) = 65536`. Если забыть поделить, вы получите Load Average равный `65536` вместо `1.0`.
2. **Контейнерная слепота.** `syscall.Sysinfo` ничего не знает про namespace контейнера. Если агент упаковать в Docker-контейнер и запустить без `pid: host`, он отдаст нагрузку и аптайм физического сервера, а не изолята.

---

## Грабли 3. `Statfs`: разница между `Bfree` и `Bavail`

Для проверки остатка дискового пространства логично использовать `syscall.Statfs`:

```go
var fs syscall.Statfs_t
if err := syscall.Statfs("/", &fs); err == nil {
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize) // Не Bfree!
}
```

### Почему именно `Bavail`?
В структуре `Statfs_t` есть два поля:
- `Bfree` — общее число свободных блоков.
- `Bavail` — число свободных блоков, доступных **непривилегированным пользователям**.

В файловых системах ext3/ext4 по умолчанию 5% пространства резервируется под `root` (чтобы демон логов или sshd не упали при заполнении диска пользователем). Если считать процент заполнения через `Bfree`, ваш мониторинг будет бодро рапортовать «свободно 4%», в то время как ваше приложение под пользователем `www-data` или `node` уже упадет с ошибкой `No space left on device`.

---

## Грабли 4. Опрос Docker через Unix Domain сокет

Тянуть официальный SDK (`github.com/docker/docker/client`) в легковесный агент — плохая идея: он тянет десятки сторонних пакетов, раздувает бинарник с 7 до 30+ МБ и увеличивает потребление памяти.

Вызывать `exec.Command("docker", "ps")` еще хуже: создание процесса каждые 5 секунд создает лишнюю нагрузку на планировщик ядра.

Docker Daemon предоставляет REST API через Unix сокет `/var/run/docker.sock`. Стандартная библиотека `net/http` в Go умеет подключаться к Unix-сокетам без внешних библиотек через кастомный `DialContext`:

```go
func NewDockerClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
			// Важно: отключаем Keep-Alive пулинг для сокетов, если опрос редкий,
			// чтобы не держать висящие файловые дескрипторы
			DisableKeepAlives: true,
		},
		Timeout: 2 * time.Second,
	}
}
```

Запрос к `/containers/json?all=1` занимает меньше миллисекунды:

```go
type ContainerSummary struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

func ListContainers(client *http.Client) ([]ContainerSummary, error) {
	// Хост в URL игнорируется, транспорт направляет трафик в unix сокет
	resp, err := client.Get("http://localhost/containers/json?all=1")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list []ContainerSummary
	return list, json.NewDecoder(resp.Body).Decode(&list)
}
```

### Грабли с безопасностью:
Права на `/var/run/docker.sock` по умолчанию — `root:docker (0660)`. Чтобы агент мог читать сокет без прав `root`:
1. Агент должен запускаться от пользователя, входящего в группу `docker`.
2. Доступ к `docker.sock` эквивалентен `root`-доступу к хосту (через запуск привилегированного контейнера с монтированием `/`). Поэтому агент должен выполнять **только чтение** либо иметь строгий white-list действий (например, только `POST /containers/{id}/restart`).

---

## Грабли 5. Кольцевой буфер (Ring Buffer) на сервере

Когда на центральный сервер сыпется телеметрия с десятков нод каждые 5 секунд, писать каждую точку в SQLite или Postgres на диск — значит быстро израсходовать ресурс дешевых SSD.

Для отображения горячих графиков (последние 1–2 часа) мы используем кольцевой буфер в оперативной памяти:

```go
type RingBuffer struct {
	mu      sync.RWMutex
	points  []Point
	head    int
	size    int
	maxSize int
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		points:  make([]Point, capacity),
		maxSize: capacity,
	}
}

func (r *RingBuffer) Push(p Point) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.points[r.head] = p
	r.head = (r.head + 1) % r.maxSize
	if r.size < r.maxSize {
		r.size++
	}
}

func (r *RingBuffer) GetAll() []Point {
	r.mu.RLock()
	defer r.mu.RUnlock()

	res := make([]Point, r.size)
	if r.size < r.maxSize {
		copy(res, r.points[:r.size])
		return res
	}
	// Буфер заполнен: собираем в хронологическом порядке от head до конца и с 0 до head
	copy(res, r.points[r.head:])
	copy(res[r.maxSize-r.head:], r.points[:r.head])
	return res
}
```

### Чего мы лишаемся при таком подходе:
- **Данные теряются при рестарте.** Если сервер мониторинга упал или перезагрузился, оперативный график за последний час обнуляется. 
- **Решение компромисса:** критические данные (инциденты, факты падения нод, изменения SLA) пишутся в SQLite с включенным WAL (`PRAGMA journal_mode=WAL;`), а высокочастотные метрики процессора и памяти живут в памяти до ротации.

---

## Сравнение профиля памяти

Результаты профилирования агента через `pprof` и замера RSS после 48 часов непрерывной работы на Debian 12:

- **Go Runtime Heap:** ~2.1 МБ
- **RSS (Resident Set Size в ОС):** **4.2 МБ**
- **CPU time:** < 0.05% от одного ядра
- **Размер бинарника (stripped, `-ldflags="-s -w"`):** **6.8 МБ**

Для сравнения: один только Node Exporter в стандартной сборке потребляет ~25–35 МБ RAM, а связка cAdvisor + Prometheus требует от 350 МБ и выше.

---

## Резюме

1. **`/proc/meminfo`:** для адекватного расчета используйте `MemAvailable`, а не `MemFree`.
2. **`syscall.Sysinfo`:** делите поля `Loads` на 65536, но помните, что вызов видит только хост.
3. **`syscall.Statfs`:** считайте свободное место по `Bavail`, иначе пропустите момент, когда диск заполнится для сервисов.
4. **Docker API:** общайтесь через Unix Domain Socket нативными средствами `net/http` — это надежнее `exec` и в 10 раз легче официального SDK.
5. **Телеметрия:** держите горячую историю метрик в памяти (Ring Buffer), сохраняя на диск только инциденты и факты смены состояний.

Весь код, описанный в статье, открыт в репозитории [github.com/NQAI-Dev/nodepulse](https://github.com/NQAI-Dev/nodepulse) под лицензией MIT.
