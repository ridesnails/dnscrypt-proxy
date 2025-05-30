package main

import (
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/jedisct1/go-sieve-cache/pkg/sievecache"
	"github.com/miekg/dns"
)

const StaleResponseTTL = 30 * time.Second

type CachedResponse struct {
	expiration time.Time
	msg        dns.Msg
}

type CachedResponses struct {
	cache     *sievecache.ShardedSieveCache[[32]byte, CachedResponse]
	cacheMu   sync.Mutex
	cacheOnce sync.Once
}

var cachedResponses CachedResponses

func computeCacheKey(pluginsState *PluginsState, msg *dns.Msg) [32]byte {
	question := msg.Question[0]
	h := sha512.New512_256()
	var tmp [5]byte
	binary.LittleEndian.PutUint16(tmp[0:2], question.Qtype)
	binary.LittleEndian.PutUint16(tmp[2:4], question.Qclass)
	if pluginsState.dnssec {
		tmp[4] = 1
	}
	h.Write(tmp[:])
	normalizedRawQName := []byte(question.Name)
	NormalizeRawQName(&normalizedRawQName)
	h.Write(normalizedRawQName)
	var sum [32]byte
	h.Sum(sum[:0])

	return sum
}

// ---

type PluginCache struct{}

func (plugin *PluginCache) Name() string {
	return "cache"
}

func (plugin *PluginCache) Description() string {
	return "DNS cache (reader)."
}

func (plugin *PluginCache) Init(proxy *Proxy) error {
	return nil
}

func (plugin *PluginCache) Drop() error {
	return nil
}

func (plugin *PluginCache) Reload() error {
	return nil
}

func (plugin *PluginCache) Eval(pluginsState *PluginsState, msg *dns.Msg) error {
	cacheKey := computeCacheKey(pluginsState, msg)

	if cachedResponses.cache == nil {
		return nil
	}
	cached, ok := cachedResponses.cache.Get(cacheKey)
	if !ok {
		return nil
	}
	expiration := cached.expiration
	synth := cached.msg.Copy()

	synth.Id = msg.Id
	synth.Response = true
	synth.Compress = true
	synth.Question = msg.Question

	if time.Now().After(expiration) {
		expiration2 := time.Now().Add(StaleResponseTTL)
		updateTTL(synth, expiration2)
		pluginsState.sessionData["stale"] = synth
		return nil
	}

	updateTTL(synth, expiration)

	pluginsState.synthResponse = synth
	pluginsState.action = PluginsActionSynth
	pluginsState.cacheHit = true
	return nil
}

// ---

type PluginCacheResponse struct{}

func (plugin *PluginCacheResponse) Name() string {
	return "cache_response"
}

func (plugin *PluginCacheResponse) Description() string {
	return "DNS cache (writer)."
}

func (plugin *PluginCacheResponse) Init(proxy *Proxy) error {
	return nil
}

func (plugin *PluginCacheResponse) Drop() error {
	return nil
}

func (plugin *PluginCacheResponse) Reload() error {
	return nil
}

func (plugin *PluginCacheResponse) Eval(pluginsState *PluginsState, msg *dns.Msg) error {
	if msg.Rcode != dns.RcodeSuccess && msg.Rcode != dns.RcodeNameError && msg.Rcode != dns.RcodeNotAuth {
		return nil
	}
	if msg.Truncated {
		return nil
	}
	cacheKey := computeCacheKey(pluginsState, msg)
	ttl := getMinTTL(
		msg,
		pluginsState.cacheMinTTL,
		pluginsState.cacheMaxTTL,
		pluginsState.cacheNegMinTTL,
		pluginsState.cacheNegMaxTTL,
	)
	cachedResponse := CachedResponse{
		expiration: time.Now().Add(ttl),
		msg:        *msg,
	}
	var cacheInitError error
	cachedResponses.cacheOnce.Do(func() {
		cache, err := sievecache.NewSharded[[32]byte, CachedResponse](pluginsState.cacheSize)
		if err != nil {
			cacheInitError = err
		} else {
			cachedResponses.cache = cache
		}
	})
	if cacheInitError != nil {
		return fmt.Errorf("failed to initialize the cache: %w", cacheInitError)
	}
	if cachedResponses.cache != nil {
		cachedResponses.cache.Insert(cacheKey, cachedResponse)
	}
	updateTTL(msg, cachedResponse.expiration)

	return nil
}
