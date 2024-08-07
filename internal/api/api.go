package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/felipeokino/websocket_go.git/internal/constants"
	"github.com/felipeokino/websocket_go.git/internal/store/pgstore"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/cors"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mu          *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q: q,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}

	r := chi.NewRouter()

	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{"Link"},
	}))

	r.Get("/subscribe/{room_id}", a.handleSubscribe)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Post("/", a.handleCreateRoomMessage)
				r.Get("/", a.handleGetRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Patch("/react", a.handleReactFromMessage)
					r.Delete("/react", a.handleRemoveReactFromMessage)
					r.Delete("/", a.handleRemoveMessage)
					r.Patch("/answer", a.handleMarkMessageAsAnswered)
				})
			})
		})
	})
	a.r = r

	return a
}

type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

func (h apiHandler) notifyClient(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subscribers, ok := h.subscribers[m.RoomID]

	if !ok || len(subscribers) == 0 {
		return
	}

	for conn, cancel := range subscribers {
		if err := conn.WriteJSON(m); err != nil {
			slog.Error("Failed to send message to client", "error", err)
			cancel()
		}
	}
}

func validateRoomID(r *http.Request) (uuid.UUID, error) {
	roomID := chi.URLParam(r, "room_id")

	if roomID == "" {
		return uuid.Nil, errors.New("room_id is required")
	}

	id, err := uuid.Parse(roomID)

	if err != nil {
		return uuid.Nil, errors.New("invalid room_id")
	}

	return id, nil
}

func validateMessageID(r *http.Request) (uuid.UUID, error) {
	messageID := chi.URLParam(r, "message_id")

	if messageID == "" {
		return uuid.Nil, errors.New("message_id is required")
	}

	id, err := uuid.Parse(messageID)

	if err != nil {
		return uuid.Nil, errors.New("invalid message_id")
	}

	return id, err
}

func (h apiHandler) roomExists(r *http.Request) (bool, error) {
	roomID, err := validateRoomID(r)
	if err != nil {
		return false, err
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	return err == nil, err
}

func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	roomID, _ := validateRoomID(r)

	_, err := h.roomExists(r)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}

		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)

	if err != nil {
		slog.Warn("Failed to upgrade connection", "error", err)
		http.Error(w, "Failed to upgrade connection", http.StatusBadRequest)
		return
	}

	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())

	h.mu.Lock()

	if _, ok := h.subscribers[roomID.String()]; !ok {
		h.subscribers[roomID.String()] = make(map[*websocket.Conn]context.CancelFunc)
	}

	slog.Info("New connection", "room_id", roomID.String(), "client_ip", r.RemoteAddr)

	h.subscribers[roomID.String()][conn] = cancel

	h.mu.Unlock()

	<-ctx.Done()

	h.mu.Lock()
	delete(h.subscribers[roomID.String()], conn)
	h.mu.Unlock()

}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	roomID, err := h.q.CreateRoom(r.Context(), body.Theme)

	if err != nil {
		slog.Error("Failed to create room", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomID.String()})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := h.q.GetRooms(r.Context())
	if err != nil {
		slog.Error("Failed to get rooms", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Room struct {
		ID    string `json:"id"`
		Theme string `json:"theme"`
	}

	type Response struct {
		Rooms []Room `json:"rooms"`
	}

	var roomsData = []Room{}

	for _, room := range rooms {
		roomsData = append(roomsData, Room{
			ID:    room.ID.String(),
			Theme: room.Theme,
		})
	}

	data, _ := json.Marshal(Response{
		Rooms: roomsData,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	roomId, _ := validateRoomID(r)

	_, err := h.roomExists(r)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}

		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	messageId, err := h.q.CreateMessage(r.Context(), pgstore.CreateMessageParams{
		RoomID:  roomId,
		Message: body.Message,
	})

	if err != nil {
		slog.Error("Failed to create message", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(Response{ID: messageId.String()})

	go h.notifyClient(Message{
		Kind:   constants.MessageKindMessageCreated,
		RoomID: roomId.String(),
		Value: MessageMessageCreated{
			ID:      messageId.String(),
			Message: body.Message,
		},
	})

	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(data)
}

func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	roomId, _ := validateRoomID(r)
	_, err := h.roomExists(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	messages, err := h.q.GetMessagesByRoom(r.Context(), roomId)

	if err != nil {
		slog.Error("Failed to get messages", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Mensages struct {
		ID             string `json:"id"`
		Message        string `json:"message"`
		ReactionsCount int64  `json:"reactions_count"`
		Answered       bool   `json:"answered"`
	}

	type Response struct {
		Messages []Mensages `json:"messages"`
		Total    int64
	}

	var messagesData = []Mensages{}

	for _, message := range messages {
		messagesData = append(messagesData, Mensages{
			ID:             message.ID.String(),
			Message:        message.Message,
			ReactionsCount: message.ReactionsCount,
			Answered:       message.Answered,
		})
	}
	data, _ := json.Marshal(Response{
		Messages: messagesData,
		Total:    int64(len(messages)),
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h apiHandler) handleReactFromMessage(w http.ResponseWriter, r *http.Request) {
	roomId, err := validateRoomID(r)

	if err != nil {
		slog.Error("Failed to get room", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	_, err = h.roomExists(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	messageId, err := validateMessageID(r)
	if err != nil {
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	reactionsCount, err := h.q.ReactToMessage(r.Context(), messageId)

	if err != nil {
		slog.Error("Failed to react to message", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Response struct {
		ID             string `json:"id"`
		ReactionsCount int64  `json:"reactions_count"`
	}

	data, _ := json.Marshal(Response{
		ID:             messageId.String(),
		ReactionsCount: reactionsCount,
	})

	type MessageReactionAdded struct {
		ID             string `json:"id"`
		ReactionsCount int64  `json:"reactions_count"`
	}

	go h.notifyClient(Message{
		Kind:   constants.MessageKindReactionAdded,
		RoomID: roomId.String(),
		Value: MessageReactionAdded{
			ID:             messageId.String(),
			ReactionsCount: reactionsCount,
		},
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	_, err := validateRoomID(r)
	_, err = h.roomExists(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}
	messageId, err := validateMessageID(r)
	if err != nil {
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	message, err := h.q.GetMessage(r.Context(), messageId)

	if err != nil {
		slog.Error("Failed to get message", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Response struct {
		ID             string `json:"id"`
		Message        string `json:"message"`
		ReactionsCount int64  `json:"reactions_count"`
		Answered       bool   `json:"answered"`
	}

	data, _ := json.Marshal(Response{
		ID:             message.ID.String(),
		Message:        message.Message,
		ReactionsCount: message.ReactionsCount,
		Answered:       message.Answered,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h apiHandler) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {
	roomId, _ := validateRoomID(r)
	_, err := h.roomExists(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	messageId, err := validateMessageID(r)
	if err != nil {
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	reactionsCount, err := h.q.RemoveReactionFromMessage(r.Context(), messageId)

	if err != nil {
		slog.Error("Failed to remove reaction from message", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Response struct {
		ID             string `json:"id"`
		ReactionsCount int64  `json:"reactions_count"`
	}

	data, _ := json.Marshal(Response{
		ID:             messageId.String(),
		ReactionsCount: reactionsCount,
	})

	type MessageReactionRemoved struct {
		ID             string `json:"id"`
		ReactionsCount int64  `json:"reactions_count"`
	}

	go h.notifyClient(Message{
		Kind:   constants.MessageKindReactionRemoved,
		RoomID: roomId.String(),
		Value: MessageReactionRemoved{
			ID:             messageId.String(),
			ReactionsCount: reactionsCount,
		},
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {
	roomId, err := validateRoomID(r)
	if err != nil {
		slog.Error("Failed to get room", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}
	_, err = h.roomExists(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	messageId, err := validateMessageID(r)
	if err != nil {
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	answered, err := h.q.MarkMessageAsRead(r.Context(), messageId)

	if err != nil {
		slog.Error("Failed to mark message as answered", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Response struct {
		ID       string `json:"id"`
		Answered bool   `json:"answered"`
	}

	data, _ := json.Marshal(Response{
		ID:       messageId.String(),
		Answered: answered,
	})

	type MessageMessageAnswered struct {
		ID       string `json:"id"`
		Answered bool   `json:"answered"`
	}

	go h.notifyClient(Message{
		Kind:   constants.MessageKindMessageAnswered,
		RoomID: roomId.String(),
		Value: MessageMessageAnswered{
			ID:       messageId.String(),
			Answered: answered,
		},
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h apiHandler) handleRemoveMessage(w http.ResponseWriter, r *http.Request) {
	roomId, _ := validateRoomID(r)
	_, err := h.roomExists(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Room not found", http.StatusNotFound)
			return
		}
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	messageId, err := validateMessageID(r)
	if err != nil {
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	_, err = h.q.RemoveMessageFromRoom(r.Context(), messageId)

	if err != nil {
		slog.Error("Failed to remove message from room", "error", err)
		http.Error(w, constants.SomethingWentWrong, http.StatusInternalServerError)
		return
	}

	type Response struct {
		Message string `json:"message"`
	}

	data, _ := json.Marshal(Response{
		Message: "Message removed",
	})

	type MessageMessageRemoved struct {
		Message string `json:"message"`
	}

	go h.notifyClient(Message{
		Kind:   constants.MessageKindMessageRemoved,
		RoomID: roomId.String(),
		Value: MessageMessageRemoved{
			Message: "Message removed",
		},
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
