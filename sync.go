package gomatrix

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"
)

// Syncer represents an interface that must be satisfied in order to do /sync requests on a client.
type Syncer interface {
	// ProcessResponse Process the /sync response. The since parameter is the since= value that was used to produce the response.
	// This is useful for detecting the very first sync (since=""). If an error is return, Syncing will be stopped
	// permanently.
	ProcessResponse(resp *RespSync, since string) error
	// OnFailedSync returns either the time to wait before retrying or an error to stop syncing permanently.
	OnFailedSync(res *RespSync, err error) (time.Duration, error)
	// GetFilterJSON for the given user ID. NOT the filter ID.
	GetFilterJSON(userID string) json.RawMessage
}

// DefaultRetryTimeout Constants for sync-related operations.
const (
	DefaultRetryTimeout = 10 * time.Second // Default retry timeout after failed sync.
)

// DefaultSyncer is the default syncing implementation. You can either write your own syncer, or selectively
// replace parts of this default syncer (e.g. the ProcessResponse method). The default syncer uses the observer
// pattern to notify callers about incoming events. See DefaultSyncer.OnEventType for more information.
type DefaultSyncer struct {
	UserID    string
	Store     Storer
	listeners map[string][]OnEventListener // event type to listeners array
}

// OnEventListener can be used with DefaultSyncer.OnEventType to be informed of incoming events.
type OnEventListener func(*Event)

// NewDefaultSyncer returns an instantiated DefaultSyncer.
func NewDefaultSyncer(userID string, store Storer) *DefaultSyncer {
	return &DefaultSyncer{
		UserID:    userID,
		Store:     store,
		listeners: make(map[string][]OnEventListener),
	}
}

// ProcessResponse processes the /sync response in a way suitable for bots. "Suitable for bots" means a stream of
// unrepeating events. Returns a fatal error if a listener panics.
func (s *DefaultSyncer) ProcessResponse(res *RespSync, since string) (err error) {
	if !s.shouldProcessResponse(res, since) {
		return err
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf(
				"ProcessResponse panicked! userID=%s since=%s panic=%s\n%s",
				s.UserID,
				since,
				r,
				debug.Stack(),
			)
		}
	}()

	for roomID, roomData := range res.Rooms.Join {
		room := s.getOrCreateRoom(roomID)
		for _, event := range roomData.State.Events {
			event.RoomID = roomID
			room.UpdateState(&event)
			s.notifyListeners(&event)
		}
		for _, event := range roomData.Timeline.Events {
			event.RoomID = roomID
			s.notifyListeners(&event)
		}
		for _, event := range roomData.Ephemeral.Events {
			event.RoomID = roomID
			s.notifyListeners(&event)
		}
	}
	for roomID, roomData := range res.Rooms.Invite {
		room := s.getOrCreateRoom(roomID)
		for _, event := range roomData.State.Events {
			event.RoomID = roomID
			room.UpdateState(&event)
			s.notifyListeners(&event)
		}
	}
	for roomID, roomData := range res.Rooms.Leave {
		room := s.getOrCreateRoom(roomID)
		for _, event := range roomData.Timeline.Events {
			if event.StateKey != nil {
				event.RoomID = roomID
				room.UpdateState(&event)
				s.notifyListeners(&event)
			}
		}
	}
	return err
}

// OnEventType allows callers to be notified when there are new events for the given event type.
// There are no duplicate checks.
func (s *DefaultSyncer) OnEventType(eventType string, callback OnEventListener) {
	_, exists := s.listeners[eventType]
	if !exists {
		s.listeners[eventType] = []OnEventListener{}
	}
	s.listeners[eventType] = append(s.listeners[eventType], callback)
}

// isJoinEventForUser checks if the given event is a room membership "join" event for the specified userID.
func isJoinEventForUser(event *Event, userID string) bool {
	if event.Type != "m.room.member" || event.StateKey == nil || *event.StateKey != userID {
		return false
	}

	m, ok := event.Content["membership"]
	if !ok {
		return false
	}

	mship, ok := m.(string)
	if !ok {
		return false
	}

	return mship == "join"
}

// shouldProcessResponse returns true if the response should be processed.
// We use the since parameter to detect if this is the first ever sync request.
// If it's the first sync request, we don't have any state to process so return false.
// We detect this by checking if the since token is empty.
//
// We also want to filter out events we've already processed.
// We use the room ID timestamp marker to do this.
//
// There is also a bug in the /sync API which causes it to return both the current
// and historical state for rooms you've JUST joined. This results in duplicates which
// we wish to filter out. This is done by inspecting a list of recent events and
// filtering out stuff that shouldn't be processed.
func (s *DefaultSyncer) shouldProcessResponse(resp *RespSync, since string) bool {
	if since == "" {
		return false
	}

	// This is a horrible hack because /sync will return the most recent messages for a room
	// as soon as you /join it. We do NOT want to process those events in that particular room
	// because they may have already been processed (if you toggle the bot in/out of the room).
	//
	// Work around this by inspecting each room's timeline and seeing if an m.room.member event for us
	// exists and is "join" and then discard processing that room entirely if so.
	// TODO: We probably want to process messages from after the last join event in the timeline.
	for roomID, roomData := range resp.Rooms.Join {
		for i := len(roomData.Timeline.Events) - 1; i >= 0; i-- {
			e := roomData.Timeline.Events[i]

			if isJoinEventForUser(&e, s.UserID) {
				_, roomExists := resp.Rooms.Join[roomID]
				if !roomExists {
					continue
				}
				delete(resp.Rooms.Join, roomID)   // don't re-process messages
				delete(resp.Rooms.Invite, roomID) // don't re-process invites
				break
			}
		}
	}
	return true
}

// getOrCreateRoom must only be called by the Sync() goroutine which calls ProcessResponse().
func (s *DefaultSyncer) getOrCreateRoom(roomID string) *Room {
	room := s.Store.LoadRoom(roomID)
	if room == nil { // create a new Room
		room = NewRoom(roomID)
		s.Store.SaveRoom(room)
	}
	return room
}

func (s *DefaultSyncer) notifyListeners(event *Event) {
	listeners, exists := s.listeners[event.Type]
	if !exists {
		return
	}
	for _, fn := range listeners {
		fn(event)
	}
}

// OnFailedSync is called when the matrix server returns a 'limited' response to the sync API call.
// Return the time to wait before attempting another sync, or an error to give up syncing.
func (s *DefaultSyncer) OnFailedSync(_ *RespSync, _ error) (time.Duration, error) {
	return DefaultRetryTimeout, nil
}

// GetFilterJSON returns a filter with a timeline limit of 50.
func (s *DefaultSyncer) GetFilterJSON(_ string) json.RawMessage {
	return json.RawMessage(`{"room":{"timeline":{"limit":50}}}`)
}
