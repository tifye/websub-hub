package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type Hub struct {
	logger        *slog.Logger
	mux           *http.ServeMux
	subscriptions map[string]*Subscription
	rwMu          sync.RWMutex
	client        *http.Client
}

func NewHub(logger *slog.Logger) *Hub {
	mux := &http.ServeMux{}

	h := &Hub{
		logger:        logger,
		mux:           mux,
		subscriptions: make(map[string]*Subscription),
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}

	mux.HandleFunc("GET /a-topic", func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("a-topic")
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte("not implemented"))
	})
	mux.HandleFunc("POST /", h.handleSubscribe)
	mux.HandleFunc("POST /publish", h.handlePublish)

	return h
}

func (h *Hub) Mux() *http.ServeMux {
	return h.mux
}

type Topic = string

type Subscription struct {
	CallbackURL   *url.URL
	Topic         string
	LeaseDeadline time.Time
	Lease         time.Duration
	secret        []byte
}

func (h *Hub) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	h.logger.Debug("/subscribe")

	if err := r.ParseForm(); err != nil {
		h.logger.Debug(err.Error())
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("error parsing body"))
		return
	}

	callback := r.PostFormValue("hub.callback")
	mode := r.PostFormValue("hub.mode")
	topic := r.PostFormValue("hub.topic")
	lease := r.PostFormValue("hub.lease_seconds")
	secret := r.PostFormValue("hub.secret")
	h.logger.Info("subscription request", "callback", callback, "mode", mode, "topic", topic, "lease", lease)

	if mode != ModeSubscribe && mode != ModeUnsubscribe {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid hub.mode value"))
		return
	}

	if topic == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("missing hub.topic"))
		return
	}

	callbackUrl, err := url.Parse(callback)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
		return
	}

	var leaseDuration time.Duration
	if lease == "" {
		leaseDuration = time.Hour
	} else {
		leaseSeconds, err := strconv.Atoi(lease)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(err.Error()))
			return
		}
		// possible overflow
		leaseDuration = time.Duration(leaseSeconds) * time.Second
	}

	subscription := Subscription{
		CallbackURL:   callbackUrl,
		Topic:         topic,
		LeaseDeadline: time.Now().Add(leaseDuration),
		Lease:         leaseDuration,
		secret:        []byte(secret),
	}

	err = h.verifySubscriptionIntent(r.Context(), &subscription, mode)
	if err != nil {
		h.logger.Debug(err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("failed intent verification"))
		return
	}

	h.rwMu.Lock()
	if mode == ModeSubscribe {
		h.subscriptions[subscription.CallbackURL.String()] = &subscription
	} else {
		delete(h.subscriptions, subscription.CallbackURL.String())
	}
	h.rwMu.Unlock()

	h.logger.Debug(fmt.Sprintf("%+v", subscription))

	w.WriteHeader(http.StatusAccepted)
	w.Write(nil)
}

type SubscriptionMode = string

var (
	ModeSubscribe   SubscriptionMode = "subscribe"
	ModeUnsubscribe SubscriptionMode = "unsubscribe"
)

func (h *Hub) verifySubscriptionIntent(ctx context.Context, sub *Subscription, mode SubscriptionMode) error {
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes) // never errors
	challenge := base64.RawURLEncoding.EncodeToString(bytes)

	u := new(url.URL)
	*u = *sub.CallbackURL
	query := u.Query()
	query.Add("hub.mode", mode)
	query.Add("hub.topic", sub.Topic)
	query.Add("hub.challenge", challenge)
	if mode == ModeSubscribe {
		query.Add("hub.lease", fmt.Sprintf("%f", sub.Lease.Seconds()))
	}
	u.RawQuery = query.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return err
	}

	req = req.WithContext(ctx)
	res, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	rChallenge, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if challenge != string(rChallenge) {
		return fmt.Errorf("invalid challenge echo")
	}

	return nil
}

func (h *Hub) handlePublish(w http.ResponseWriter, r *http.Request) {
	h.logger.Debug("/publish")

	h.rwMu.RLock()
	subs := make([]*Subscription, 0, len(h.subscriptions))
	for _, s := range h.subscriptions {
		subs = append(subs, s)
	}
	h.rwMu.RUnlock()

	message := struct {
		Meep string `json:"meep"`
		Mino string `json:"mino"`
	}{
		Meep: "meep",
		Mino: "mino",
	}
	messageData, _ := json.Marshal(message)

	for _, s := range subs {
		callbackStr := s.CallbackURL.String()

		req, err := http.NewRequest("POST", s.CallbackURL.String(), bytes.NewReader(messageData))
		if err != nil {
			h.logger.Error(err.Error(), "callback", callbackStr, "err", err)
			continue
		}

		if len(s.secret) > 0 {
			mac := hmac.New(sha256.New, s.secret)
			if _, err := mac.Write(messageData); err != nil {
				h.logger.Error(err.Error(), "callback", callbackStr)
			}
			sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			req.Header.Add("X-Hub-Signature", sig)
			h.logger.Debug(sig)
		}

		res, err := h.client.Do(req)
		if err != nil {
			h.logger.Error("failed to notify subscriber", "callback", callbackStr, "err", err)
			continue
		}
		defer res.Body.Close()

		if res.StatusCode < 200 || res.StatusCode > 299 {
			h.logger.Error("callback returned non-2xx status", "callback", callbackStr, "status", res.Status)
		}
	}

}
