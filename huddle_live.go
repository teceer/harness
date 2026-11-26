package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/teceer/harness/internal/store"
	"github.com/teceer/harness/internal/transcript"
)

// liveConn is the voice model's connection (a *websocket.Conn to Gemini
// Live; a fake in tests).
type liveConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	CloseNow() error
}

type liveState struct {
	conn      liveConn
	muted     bool
	ending    bool
	playUntil time.Time // when the phone finishes playing what it got
	asks      int
}

// serveLive runs one phone connection. The first one starts the voice; a
// later one (the phone reconnected) takes over its audio.
func (m *huddleManager) serveLive(h *huddle, phone *websocket.Conn) {
	h.mu.Lock()
	if old := h.phone; old != nil {
		old.Close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
	h.phone = phone
	h.phoneGen++
	gen := h.phoneGen
	first := h.stop == nil
	ls := &liveState{}
	if first {
		ctx, cancel := context.WithCancel(context.Background())
		h.stop = cancel
		h.status = hLive
		h.live = ls
		go m.bridge(ctx, h, ls)
	}
	h.mu.Unlock()
	if first {
		m.toPhone(h, map[string]string{"type": "state", "state": "connecting"})
	} else {
		m.toPhone(h, map[string]string{"type": "state", "state": "live"})
	}

	m.readPhone(h, phone)

	// The phone went away without hanging up (network): give it a moment
	// to come back before ending the call.
	h.mu.Lock()
	current := h.phoneGen == gen && h.final == nil
	if current {
		h.phone = nil
	}
	h.mu.Unlock()
	if current {
		time.AfterFunc(20*time.Second, func() {
			h.mu.Lock()
			gone := h.phoneGen == gen && h.phone == nil
			h.mu.Unlock()
			if gone {
				m.finish(h, hEnded, "hangup")
			}
		})
	}
}

func (m *huddleManager) readPhone(h *huddle, phone *websocket.Conn) {
	ctx := context.Background()
	for {
		typ, b, err := phone.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			h.mu.Lock()
			ls := h.live
			var conn liveConn
			if ls != nil && !ls.muted {
				conn = ls.conn
			}
			h.mu.Unlock()
			if conn != nil {
				writeJSONWS(ctx, conn, map[string]any{"realtimeInput": map[string]any{
					"audio": map[string]any{"data": b, "mimeType": "audio/pcm;rate=16000"}}})
			}
			continue
		}
		var msg struct {
			Type string
			On   bool
		}
		if json.Unmarshal(b, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "hangup":
			m.finish(h, hEnded, "hangup")
			return
		case "mute":
			h.mu.Lock()
			var conn liveConn
			if h.live != nil {
				h.live.muted = msg.On
				conn = h.live.conn
			}
			h.mu.Unlock()
			if msg.On && conn != nil { // flush what the model heard so far
				writeJSONWS(ctx, conn, map[string]any{"realtimeInput": map[string]any{"audioStreamEnd": true}})
			}
		}
	}
}

func (m *huddleManager) toPhone(h *huddle, v any) {
	h.mu.Lock()
	p := h.phone
	h.mu.Unlock()
	if p != nil {
		writeJSONWS(context.Background(), p, v)
	}
}

func (m *huddleManager) audioToPhone(h *huddle, ls *liveState, pcm []byte) {
	h.mu.Lock()
	p := h.phone
	now := time.Now()
	if ls.playUntil.Before(now) {
		ls.playUntil = now
	}
	ls.playUntil = ls.playUntil.Add(time.Duration(len(pcm)) * time.Second / 48000) // 24 kHz, 16-bit
	h.mu.Unlock()
	if p != nil {
		p.Write(context.Background(), websocket.MessageBinary, pcm)
	}
}

// say hands the voice a message that is not the user's speech.
func (m *huddleManager) say(h *huddle, ls *liveState, text string) {
	h.mu.Lock()
	conn := ls.conn
	h.mu.Unlock()
	if conn != nil {
		writeJSONWS(context.Background(), conn, map[string]any{"realtimeInput": map[string]any{"text": text}})
	}
}

// bridge keeps the voice session up for the length of the call, resuming
// it when Gemini rotates the connection, and relays the agent's answers.
func (m *huddleManager) bridge(ctx context.Context, h *huddle, ls *liveState) {
	system, tools := m.systemPrompt(h), m.tools(h)
	handle := ""
	started := false
	fails := 0
	for ctx.Err() == nil {
		conn, err := m.dial(ctx, system, tools, handle)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("huddle %s: voice: %v", short(h.ID), err)
			if fails++; !started || fails > 3 {
				m.toPhone(h, map[string]string{"type": "transcript", "role": "agent", "text": "Nie udało się połączyć z modelem głosowym."})
				m.finish(h, hEnded, "error")
				return
			}
			time.Sleep(time.Second)
			continue
		}
		fails = 0
		h.mu.Lock()
		ls.conn = conn
		h.mu.Unlock()
		if !started {
			started = true
			m.toPhone(h, map[string]string{"type": "state", "state": "live"})
			m.say(h, ls, m.kickoff(h))
			go m.relayAnswers(ctx, h, ls)
		}
		m.pump(ctx, h, ls, conn, &handle)
		conn.CloseNow()
		h.mu.Lock()
		ls.conn = nil
		h.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if handle == "" {
			m.finish(h, hEnded, "error")
			return
		}
	}
}

func (m *huddleManager) relayAnswers(ctx context.Context, h *huddle, ls *liveState) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-h.answers:
			h.addLine("agent", a)
			m.toPhone(h, map[string]string{"type": "transcript", "role": "agent", "text": a})
			m.say(h, ls, "[AGENT] Odpowiedź agenta (przekaż ją użytkownikowi własnymi słowami, krótko): "+a)
		}
	}
}

// pump handles one voice connection until it closes or Gemini asks to move.
func (m *huddleManager) pump(ctx context.Context, h *huddle, ls *liveState, conn liveConn, handle *string) {
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg liveServerMsg
		if json.Unmarshal(b, &msg) != nil {
			continue
		}
		if u := msg.SessionResumptionUpdate; u != nil && u.Resumable && u.NewHandle != "" {
			*handle = u.NewHandle
		}
		if sc := msg.ServerContent; sc != nil {
			if sc.ModelTurn != nil {
				for _, p := range sc.ModelTurn.Parts {
					if p.InlineData != nil && strings.HasPrefix(p.InlineData.MimeType, "audio/pcm") && len(p.InlineData.Data) > 2 {
						m.audioToPhone(h, ls, p.InlineData.Data)
					}
				}
			}
			if t := sc.InputTranscription; t != nil && t.Text != "" {
				h.addLine("user", t.Text)
				m.toPhone(h, map[string]string{"type": "transcript", "role": "user", "text": t.Text})
			}
			if t := sc.OutputTranscription; t != nil && t.Text != "" {
				h.addLine("model", t.Text)
				m.toPhone(h, map[string]string{"type": "transcript", "role": "model", "text": t.Text})
			}
			if sc.Interrupted {
				h.mu.Lock()
				ls.playUntil = time.Now()
				h.mu.Unlock()
				m.toPhone(h, map[string]string{"type": "interrupted"})
			}
			if sc.TurnComplete {
				h.mu.Lock()
				ending, until := ls.ending, ls.playUntil
				h.mu.Unlock()
				if ending { // let the goodbye play out
					time.AfterFunc(time.Until(until)+700*time.Millisecond, func() { m.finish(h, hEnded, "agent") })
				}
			}
		}
		if tc := msg.ToolCall; tc != nil {
			var responses []map[string]any
			for _, fc := range tc.FunctionCalls {
				responses = append(responses, map[string]any{"id": fc.ID, "name": fc.Name,
					"response": map[string]any{"result": m.callTool(ctx, h, ls, fc.Name, fc.Args)}})
			}
			writeJSONWS(ctx, conn, map[string]any{"toolResponse": map[string]any{"functionResponses": responses}})
		}
		if msg.GoAway != nil {
			return // reconnect with the handle
		}
	}
}

func (m *huddleManager) callTool(ctx context.Context, h *huddle, ls *liveState, name string, args map[string]string) string {
	switch name {
	case "ask_agent":
		q := strings.TrimSpace(args["question"])
		if q == "" {
			return "Brak pytania."
		}
		h.addLine("agent", "(pytanie do agenta) "+q)
		m.toPhone(h, map[string]string{"type": "transcript", "role": "agent", "text": "(pytanie do agenta) " + q})
		if h.Origin == originAgent {
			h.push(agentEvent{Type: "question", Question: q})
		} else {
			h.mu.Lock()
			ls.asks++
			n := ls.asks
			h.mu.Unlock()
			go m.askSession(ctx, h, q, n)
		}
		return "Pytanie wysłane do agenta. Odpowiedź przyjdzie później jako wiadomość [AGENT] (zwykle po kilkudziesięciu sekundach). Powiedz użytkownikowi, że agent sprawdza, i rozmawiajcie dalej."
	case "save_decision":
		d := strings.TrimSpace(args["decision"])
		if d == "" {
			return "Brak treści."
		}
		h.mu.Lock()
		h.decisions = append(h.decisions, trim(d, 600))
		h.mu.Unlock()
		return "Zapisane."
	case "end_call":
		h.mu.Lock()
		ls.ending = true
		h.mu.Unlock()
		time.AfterFunc(20*time.Second, func() { m.finish(h, hEnded, "agent") }) // if no turn completes
		return "Kończę rozmowę po twoim pożegnaniu. Powiedz jedno krótkie zdanie na pożegnanie."
	}
	return "Nieznane narzędzie."
}

// askSession puts the question to the session (the user called it, so no
// agent is waiting on …/next) and relays its reply once it stops working.
func (m *huddleManager) askSession(ctx context.Context, h *huddle, q string, n int) {
	marker := fmt.Sprintf("[huddle %s/%d]", short(h.ID), n)
	err := m.tellSession(h, marker+" Pytanie z rozmowy głosowej z użytkownikiem: "+q+
		" Odpowiedz krótko, kilkoma zdaniami, bez kodu i tabel: odpowiedź zostanie przeczytana na głos.")
	if err != nil {
		h.answers <- "Nie mogę teraz zapytać sesji (" + err.Error() + ")."
		return
	}
	deadline := time.Now().Add(6 * time.Minute)
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		se, err := m.st.Find(h.SessionID)
		if err != nil {
			continue
		}
		msgs, _ := transcript.Tail(se.TranscriptPath, 40)
		at := -1
		for i, x := range msgs {
			if x.Role == "user" && strings.Contains(x.Text, marker) {
				at = i
			}
		}
		if at < 0 {
			continue
		}
		var reply []string
		for _, x := range msgs[at+1:] {
			if x.Role == "assistant" && strings.TrimSpace(x.Text) != "" {
				reply = append(reply, strings.TrimSpace(x.Text))
			}
		}
		if len(reply) > 0 && se.Status != store.Working {
			select {
			case h.answers <- trim(strings.Join(reply, "\n"), 3000):
			case <-ctx.Done():
			}
			return
		}
	}
	h.answers <- "Agent nie odpowiedział w ciągu kilku minut na pytanie: " + q
}

func (m *huddleManager) kickoff(h *huddle) string {
	if h.Origin == originAgent {
		return "[SYSTEM] Użytkownik odebrał telefon. Przywitaj się jednym krótkim zdaniem, powiedz po co dzwonisz i zadaj pierwsze pytanie."
	}
	return "[SYSTEM] Użytkownik zadzwonił do ciebie. Przywitaj się jednym krótkim zdaniem, powiedz w jednym zdaniu, nad czym teraz pracujesz, i zapytaj, w czym pomóc."
}

func (m *huddleManager) tools(h *huddle) []liveTool {
	return []liveTool{
		{Name: "ask_agent", Description: "Zadaj pytanie agentowi programistycznemu, który ma dostęp do kodu i sesji. Używaj, gdy odpowiedzi nie ma w briefingu (stan kodu, szczegóły implementacji, wyniki testów) albo gdy użytkownik prosi, żeby agent coś sprawdził. Odpowiedź przychodzi później jako wiadomość [AGENT].",
			Parameters: stringParams(true, map[string]string{"question": "Samodzielne, konkretne pytanie do agenta."})},
		{Name: "save_decision", Description: "Zapisz jedną decyzję, ustalenie albo polecenie użytkownika, gdy tylko padnie. Agent dostanie je po rozmowie.",
			Parameters: stringParams(true, map[string]string{"decision": "Decyzja jednym konkretnym zdaniem."})},
		{Name: "end_call", Description: "Zakończ rozmowę, gdy wszystko omówione i użytkownik się żegna albo prosi o rozłączenie."},
	}
}

func (m *huddleManager) systemPrompt(h *huddle) string {
	var b strings.Builder
	if h.Origin == originAgent {
		fmt.Fprintf(&b, "Jesteś głosem agenta programistycznego (Claude Code), który pracuje w sesji „%s”. Agent zadzwonił do użytkownika w sprawie: %s.\n\n", h.Caller, h.Topic)
		b.WriteString("Briefing od agenta:\n" + h.Brief + "\n")
		if len(h.Questions) > 0 {
			b.WriteString("\nPytania do omówienia (po kolei):\n")
			for _, q := range h.Questions {
				b.WriteString("- " + q + "\n")
			}
		}
	} else {
		fmt.Fprintf(&b, "Jesteś głosem agenta programistycznego (Claude Code), który pracuje w sesji „%s”. Użytkownik do ciebie zadzwonił, żeby porozmawiać o tej pracy: przedyskutować pomysł, coś ustalić albo zlecić.\n\n", h.Caller)
		b.WriteString("Kontekst sesji:\n" + h.Brief + "\n")
		b.WriteString("\nPo rozmowie zapisane ustalenia trafią do agenta jako polecenie, więc zapisuj je dokładnie.\n")
	}
	b.WriteString(`
Zasady rozmowy:
- Mówisz wyłącznie po polsku, przez całą rozmowę. Rozmówcy mówią po polsku: gdy ich wypowiedź wygląda na inny język (hiszpański, francuski, angielski) albo nie ma sensu, to błąd rozpoznawania mowy przez szum, odległość od mikrofonu lub kilka osób naraz. Nigdy nie przechodź na inny język, nie pytaj o zmianę języka i nie komentuj języka. Powiedz krótko, że nie dosłyszałeś, i poproś o powtórzenie.
- Mówisz naturalnie i zwięźle, jak kolega z zespołu w huddle na Slacku. Bez list i formatowania, jedno pytanie naraz. Nie zakładaj płci rozmówcy: mów bezosobowo albo w formach neutralnych.
- Przy brainstormingu proponuj konkretne opcje z rekomendacją i krótkim uzasadnieniem.
- Agent ma szerokie możliwości na komputerze użytkownika: terminal, repozytoria, przeglądarkę z zalogowanymi kontami, pocztę i inne narzędzia. Ty nie wiesz, do czego ma dostęp, więc nigdy nie mów, że agent czegoś nie może albo do czegoś nie ma dostępu, i nie odsyłaj użytkownika, żeby zrobił coś sam. Każdą prośbę o sprawdzenie albo zrobienie czegoś od razu przekaż przez ask_agent, dokładnie tak, jak użytkownik ją wypowiedział, bez dopisywania własnych wniosków. Przekaż odpowiedź agenta, nawet gdy przeczy temu, co wcześniej sądziłeś.
- Nie zmyślaj faktów o kodzie, narzędziach ani stanie pracy. Gdy czegoś nie wiesz, użyj ask_agent, powiedz, że agent to sprawdza, i rozmawiaj dalej. Gdy przyjdzie [AGENT], przekaż odpowiedź własnymi słowami.
- Decyzje i polecenia zapisuj przez save_decision tylko wtedy, gdy są jasne. Gdy wypowiedź jest niejasna, urwana albo sama sobie przeczy, dopytaj, zanim cokolwiek zapiszesz. Polecenie, które usuwa, nadpisuje, wdraża albo wysyła coś na zewnątrz, zawsze powtórz własnymi słowami i zapisz dopiero po wyraźnym potwierdzeniu.
- Decyzje podejmuje tylko osoba, z którą agent rozmawia od początku. Gdy powie, że do rozmowy dołącza ktoś inny albo że przekazuje mu telefon, ta osoba nie zna kontekstu: przywitaj się, krótko powiedz, kim jesteś, i odpowiadaj na jej pytania, ale nie przechodź z nią przez listę decyzji i nie zapisuj jej słów jako decyzji. Do tematów rozmowy wracasz dopiero, gdy wróci pierwsza osoba.
- Gdy wszystko omówione: podsumuj ustalenia w dwóch, trzech zdaniach, zapytaj, czy coś jeszcze, pożegnaj się i wywołaj end_call. Po pożegnaniu już nic nie mów.
- Wiadomości zaczynające się od [SYSTEM] lub [AGENT] nie pochodzą od użytkownika, nie mów o nich jak o wypowiedzi użytkownika.
`)
	return b.String()
}
