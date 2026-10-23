package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The stand-in voice server: a Discord voice gateway on wss://127.0.0.1:9443,
// its UDP side on 9445, and a control API on http://127.0.0.1:9444 that
// says when each server's first packet came, and how evenly the rest did.
const (
	gatewayAddr = "127.0.0.1:9443"
	udpPort     = 9445
	controlAddr = "127.0.0.1:9444"
)

type packet struct {
	at   time.Time
	size int
}

type session struct {
	guild     string
	ssrc      uint32
	described chan struct{} // closed once the session description went out
	packets   []packet
}

type server struct {
	mu       sync.Mutex
	byGuild  map[string]*session
	bySSRC   map[uint32]*session
	nextSSRC uint32
}

// serve runs the voice server until it's stopped.
func serve(certFile string) {
	s := &server{byGuild: map[string]*session{}, bySSRC: map[uint32]*session{}, nextSSRC: 1000}
	cert := selfSigned(certFile)

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udpPort})
	if err != nil {
		log.Fatal(err)
	}
	go s.readUDP(udp)

	gw := &http.Server{Addr: gatewayAddr, Handler: http.HandlerFunc(s.gateway),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	go func() { log.Fatal(gw.ListenAndServeTLS("", "")) }()

	mux := http.NewServeMux()
	mux.HandleFunc("/ready", s.ready)
	mux.HandleFunc("/first", s.first)
	mux.HandleFunc("/stats", s.stats)
	mux.HandleFunc("/mark", mark)
	mux.HandleFunc("/usage", usageOf)
	log.Printf("voice server on wss://%s, udp %d, control http://%s", gatewayAddr, udpPort, controlAddr)
	log.Fatal(http.ListenAndServe(controlAddr, mux))
}

// selfSigned makes a certificate for 127.0.0.1 and localhost, and writes it to
// certFile for the clients to trust.
func selfSigned(certFile string) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		log.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

type message struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

// gateway speaks just enough of Discord's voice gateway, versions 4 to 8,
// for a client to start sending audio: no DAVE, which it says it doesn't do.
func (s *server) gateway(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	send := func(op int, d any) {
		if err := c.WriteJSON(map[string]any{"op": op, "d": d}); err != nil {
			log.Printf("write: %v", err)
		}
	}
	send(8, map[string]any{"heartbeat_interval": 13750})
	var sess *session
	for {
		kind, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			continue // DAVE, which was never offered
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Op {
		case 0: // identify
			var id struct {
				ServerID string `json:"server_id"`
			}
			json.Unmarshal(m.D, &id)
			s.mu.Lock()
			s.nextSSRC++
			sess = &session{guild: id.ServerID, ssrc: s.nextSSRC, described: make(chan struct{})}
			s.byGuild[id.ServerID] = sess
			s.bySSRC[sess.ssrc] = sess
			s.mu.Unlock()
			log.Printf("guild %s identified (%s), ssrc %d", id.ServerID, r.URL.RawQuery, sess.ssrc)
			send(2, map[string]any{"ssrc": sess.ssrc, "ip": "127.0.0.1", "port": udpPort,
				"modes": []string{"aead_aes256_gcm_rtpsize", "aead_xchacha20_poly1305_rtpsize"}, "heartbeat_interval": 13750})
		case 1: // select protocol
			var sp struct {
				Data struct {
					Mode string `json:"mode"`
				} `json:"data"`
			}
			json.Unmarshal(m.D, &sp)
			key := make([]int, 32)
			for i := range key {
				key[i] = i * 7 % 256
			}
			send(4, map[string]any{"mode": sp.Data.Mode, "secret_key": key, "audio_codec": "opus",
				"media_session_id": "bench", "dave_protocol_version": 0})
			if sess != nil {
				log.Printf("guild %s ready, mode %s", sess.guild, sp.Data.Mode)
				close(sess.described)
			}
		case 3: // heartbeat
			var hb any
			json.Unmarshal(m.D, &hb)
			if v8, ok := hb.(map[string]any); ok {
				send(6, map[string]any{"t": v8["t"]})
			} else {
				send(6, hb)
			}
		}
	}
}

// readUDP answers IP discovery and notes when each audio packet came.
func (s *server) readUDP(c *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		at := time.Now()
		b := buf[:n]
		if n == 74 && binary.BigEndian.Uint16(b) == 1 { // IP discovery
			reply := make([]byte, 74)
			binary.BigEndian.PutUint16(reply, 2)
			binary.BigEndian.PutUint16(reply[2:], 70)
			copy(reply[4:8], b[4:8])
			copy(reply[8:72], from.IP.String())
			binary.BigEndian.PutUint16(reply[72:], uint16(from.Port))
			c.WriteToUDP(reply, from)
			continue
		}
		if n < 12 || b[0]&0xC0 != 0x80 || b[1]&0x7F != 0x78 {
			continue // not an audio packet, like a keepalive
		}
		s.mu.Lock()
		if sess := s.bySSRC[binary.BigEndian.Uint32(b[8:12])]; sess != nil {
			sess.packets = append(sess.packets, packet{at, n})
		}
		s.mu.Unlock()
	}
}

func (s *server) session(guild string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byGuild[guild]
}

// ready waits up to 30 s for a guild's client to be ready to send audio.
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	guild := r.URL.Query().Get("guild")
	deadline := time.Now().Add(30 * time.Second)
	for ; time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if sess := s.session(guild); sess != nil {
			select {
			case <-sess.described:
				fmt.Fprint(w, "ready")
				return
			case <-time.After(time.Until(deadline)):
			}
		}
	}
	http.Error(w, "never connected", http.StatusGatewayTimeout)
}

// first waits for the first audio packet from a guild that came after the
// time given, in unix nanoseconds.
func (s *server) first(w http.ResponseWriter, r *http.Request) {
	guild := r.URL.Query().Get("guild")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		s.mu.Lock()
		sess := s.byGuild[guild]
		if sess != nil {
			for _, p := range sess.packets {
				if p.at.UnixNano() > after {
					s.mu.Unlock()
					json.NewEncoder(w).Encode(map[string]any{"at": p.at.UnixNano(), "size": p.size})
					return
				}
			}
		}
		s.mu.Unlock()
	}
	http.Error(w, "no audio", http.StatusGatewayTimeout)
}

// stats says how evenly a guild's packets came between two times: how many,
// and the gaps between them.
func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	s.mu.Lock()
	var in []packet
	if sess := s.byGuild[q.Get("guild")]; sess != nil {
		for _, p := range sess.packets {
			if t := p.at.UnixNano(); t >= from && t <= to {
				in = append(in, p)
			}
		}
	}
	s.mu.Unlock()
	var gaps []float64
	late := 0
	for i := 1; i < len(in); i++ {
		g := float64(in[i].at.Sub(in[i-1].at).Microseconds()) / 1000
		gaps = append(gaps, g)
		if g > 30 {
			late++
		}
	}
	slices.Sort(gaps)
	out := map[string]any{"packets": len(in), "late": late}
	if len(gaps) > 0 {
		out["maxGapMs"] = gaps[len(gaps)-1]
		out["p99GapMs"] = gaps[len(gaps)*99/100]
	}
	json.NewEncoder(w).Encode(out)
}
