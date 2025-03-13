package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
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
}

func NewHub(logger *slog.Logger) *Hub {
	mux := &http.ServeMux{}

	h := &Hub{
		logger:        logger,
		mux:           mux,
		subscriptions: make(map[string]*Subscription),
	}

	mux.HandleFunc("GET /a-topic", func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("a-topic")
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte("not implemented"))
	})
	mux.HandleFunc("POST /", h.handleSubscribe)

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
	HMAC          *hash.Hash
}

func (h *Hub) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	h.logger.Debug("post index")

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
	secret := r.PostFormValue("secret")
	h.logger.Info("subscription request", "callback", callback, "mode", mode, "topic", topic, "lease", lease)

	if mode != ModeSubscribe && mode != ModeUnsubscribe {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid hub.mode value"))
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

	var mac hash.Hash
	if secret != "" {
		mac = hmac.New(sha256.New, []byte(secret))
	}

	subscription := Subscription{
		CallbackURL:   callbackUrl,
		Topic:         topic,
		LeaseDeadline: time.Now().Add(leaseDuration),
		Lease:         leaseDuration,
		HMAC:          &mac,
	}

	err = h.verifySubscriptionIntent(r.Context(), &subscription, mode)
	if err != nil {
		h.logger.Debug(err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("failed intent verification"))
		return
	}

	h.rwMu.Lock()
	h.subscriptions[subscription.CallbackURL.String()] = &subscription
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
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	rChallenge, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if challenge != string(rChallenge) {
		return fmt.Errorf("invalid challenge echo")
	}

	return nil
}
