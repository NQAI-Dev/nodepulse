package alerter

// Notifier is the storage-layer abstraction over outbound user-facing
// messages. PersistentStore depends on this interface rather than the
// concrete Telegram Dispatcher so tests can swap in a recorder without
// hitting api.telegram.org.
//
// ponytail: keep the surface tiny (incident + resolved is enough today).
// Add new methods when a new incident-derived notification category shows up
// rather than folding everything into a single Send(text, target).
type Notifier interface {
	NotifyIncidentTo(chatID int64, nodeID, severity, title, detail string)
	NotifyResolvedTo(chatID int64, nodeID, severity, title string)
}

// RichNotifier is an optional extension dispatchers can implement when
// they support inline-keyboard callbacks. The store type-asserts and falls
// back to the plain notifier path otherwise. Keeping the method on a
// separate interface avoids forcing every test double to grow buttons.
type RichNotifier interface {
	Notifier
	NotifyIncidentWithButtonsTo(chatID int64, incidentID, nodeID, severity, title, detail string)
}

// Compiles-only assertions: the production Dispatcher satisfies Notifier.
var _ Notifier = (*Dispatcher)(nil)

// RecordingNotifier captures every dispatch call for assertion in tests.
type RecordingNotifier struct {
	Incidents []NotifyCall
	Resolves  []NotifyCall
}

type NotifyCall struct {
	ChatID   int64
	NodeID   string
	Severity string
	Title    string
	Detail   string
}

func (r *RecordingNotifier) NotifyIncidentTo(chatID int64, nodeID, severity, title, detail string) {
	r.Incidents = append(r.Incidents, NotifyCall{ChatID: chatID, NodeID: nodeID, Severity: severity, Title: title, Detail: detail})
}

func (r *RecordingNotifier) NotifyResolvedTo(chatID int64, nodeID, severity, title string) {
	r.Resolves = append(r.Resolves, NotifyCall{ChatID: chatID, NodeID: nodeID, Severity: severity, Title: title})
}

// NotifyIncidentWithButtonsTo is recorded verbatim on Incidents; tests can
// tell a plain dispatch from a buttoned one by checking IncidentID != "".
func (r *RecordingNotifier) NotifyIncidentWithButtonsTo(chatID int64, incidentID, nodeID, severity, title, detail string) {
	r.Incidents = append(r.Incidents, NotifyCall{ChatID: chatID, NodeID: nodeID, Severity: severity, Title: title, Detail: incidentID + "|" + detail})
}
