package imessage

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Incoming is represents a message from someone. This struct is filled out
// and sent to incoming callback methods and/or to bound channels.
type Incoming struct {
	RowID int64  // RowID is the unique database row id.
	From  string // From is the handle of the user who sent the message.
	Text  string // Text is the body of the message.
	File  bool   // File is true if a file is attached. (no way to access it atm)
}

// Callback is the type used to return an incoming message to the consuming app.
// Create a function that matches this interface to process incoming messages
// using a callback (as opposed to a channel).
type Callback func(msg Incoming)

type chanBinding struct {
	Match string
	Chan  chan Incoming
}

type funcBinding struct {
	Match string
	Func  Callback
}

type binds struct {
	Funcs []*funcBinding
	Chans []*chanBinding
	// locks either or both slices
	sync.RWMutex
}

// IncomingChan connects a channel to a matched string in a message.
// Similar to the IncomingCall method, this will send an incoming message
// to a channel. Any message with text matching `match` is sent. Regexp supported.
// Use '.*' for all messages. The channel blocks, so avoid long operations.
func (m *Messages) IncomingChan(match string, channel chan Incoming) {
	m.binds.Lock()
	defer m.binds.Unlock()
	m.binds.Chans = append(m.binds.Chans, &chanBinding{Match: match, Chan: channel})
}

// IncomingCall connects a callback function to a matched string in a message.
// This methods creates a callback that is run in a go routine any time
// a message containing `match` is found. Use '.*' for all messages. Supports regexp.
func (m *Messages) IncomingCall(match string, callback Callback) {
	m.binds.Lock()
	defer m.binds.Unlock()
	m.binds.Funcs = append(m.binds.Funcs, &funcBinding{Match: match, Func: callback})
}

// RemoveChan deletes a message match to channel made with IncomingChan()
func (m *Messages) RemoveChan(match string) int {
	m.binds.Lock()
	defer m.binds.Unlock()
	removed := 0
	for i, rlen := 0, len(m.binds.Chans); i < rlen; i++ {
		j := i - removed
		if m.binds.Chans[j].Match == match {
			m.binds.Chans = append(m.binds.Chans[:j], m.binds.Chans[j+1:]...)
			removed++
		}
	}
	return removed
}

// RemoveCall deletes a message match to function callback made with IncomingCall()
func (m *Messages) RemoveCall(match string) int {
	m.binds.Lock()
	defer m.binds.Unlock()
	removed := 0
	for i, rlen := 0, len(m.binds.Funcs); i < rlen; i++ {
		j := i - removed
		if m.binds.Funcs[j].Match == match {
			m.binds.Funcs = append(m.binds.Funcs[:j], m.binds.Funcs[j+1:]...)
			removed++
		}
	}
	return removed
}

// processIncomingMessages starts the iMessage-sqlite3 db watcher routine(s).
func (m *Messages) processIncomingMessages() {
	go func() {
		for {
			select {
			case msg := <-m.inChan:
				m.callBacks(msg)
				m.mesgChans(msg)
			case <-m.stopChan:
				return
			}
		}
	}()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		_ = m.checkErr(err, "fsnotify failed, polling instead")
		go m.pollSQL()
		return
	}
	go m.fsnotifySQL(watcher)
	if err = watcher.Add(filepath.Dir(m.SQLPath)); err != nil {
		_ = m.checkErr(err, "fsnotify watcher failed")
	}
}

func (m *Messages) fsnotifySQL(watcher *fsnotify.Watcher) {
	var last time.Time
	defer func() {
		_ = watcher.Close()
	}()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				m.ErrorLog.Print("fsnotify watcher failed. incoming message routines stopped")
				m.Stop()
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write &&
				last.Add(m.Interval.Duration).Before(time.Now()) {
				m.DebugLog.Printf("modified file: %v", event.Name)
				last = time.Now()
				m.checkForNewMessages()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				m.ErrorLog.Print("fsnotify watcher errors failed. incoming message routines stopped.")
				m.Stop()
				return
			}
			_ = m.checkErr(err, "fsnotify watcher")
		case <-m.stopChan:
			return
		}
	}
}

func (m *Messages) pollSQL() {
	ticker := time.NewTicker(m.Interval.Round(time.Second))
	for {
		select {
		case <-ticker.C:
			m.checkForNewMessages()
		case <-m.stopChan:
			return
		}
	}
}

func (m *Messages) checkForNewMessages() {
	if m.getDB() != nil {
		return // error
	}
	defer m.closeDB()
	sql := `SELECT message.rowid as rowid, handle.id as handle, cache_has_attachments, message.text as text ` +
		`FROM message INNER JOIN handle ON message.handle_id = handle.ROWID ` +
		`WHERE is_from_me=0 AND message.rowid > $id ORDER BY message.date ASC`
	query := m.db.Prep(sql)
	query.SetInt64("$id", m.currentID)
	defer func() {
		_ = m.checkErr(query.Finalize(), sql)
	}()
	for {
		if hasRow, err := query.Step(); err != nil || !hasRow {
			_ = m.checkErr(err, sql)
			return
		}
		m.currentID = query.GetInt64("rowid")
		msg := Incoming{
			RowID: m.currentID,
			From:  strings.TrimSpace(query.GetText("handle")),
			Text:  strings.TrimSpace(query.GetText("text")),
		}
		if query.GetInt64("cache_has_attachments") == 1 {
			msg.File = true
		}
		m.inChan <- msg
		m.DebugLog.Printf("new message id %d from: %s size: %d", msg.RowID, msg.From, len(msg.Text))
	}
}

func (m *Messages) getCurrentID() error {
	sql := `SELECT MAX(rowid) AS id FROM message`
	if err := m.getDB(); err != nil {
		return err
	}
	defer m.closeDB()
	query := m.db.Prep(sql)
	defer func() {
		_ = m.checkErr(query.Finalize(), sql)
	}()
	m.DebugLog.Print("querying current id")
	hasrow, err := query.Step()
	_ = m.checkErr(err, sql)
	if hasrow && err == nil {
		m.currentID = query.GetInt64("id")
		return nil
	}
	return errors.New("no message rows found")
}

func (m *Messages) callBacks(msg Incoming) {
	m.binds.RLock()
	defer m.binds.RUnlock()
	for _, bind := range m.binds.Funcs {
		matched, err := regexp.MatchString(bind.Match, msg.Text)
		if err = m.checkErr(err, bind.Match); err == nil && matched {
			m.DebugLog.Printf("found matching message handler func: %v", bind.Match)
			go bind.Func(msg)
		}
	}
}

func (m *Messages) mesgChans(msg Incoming) {
	m.binds.RLock()
	defer m.binds.RUnlock()
	for _, bind := range m.binds.Chans {
		matched, err := regexp.MatchString(bind.Match, msg.Text)
		if err = m.checkErr(err, bind.Match); err == nil && matched {
			m.DebugLog.Printf("found matching message handler chan: %v", bind.Match)
			bind.Chan <- msg
		}
	}
}
