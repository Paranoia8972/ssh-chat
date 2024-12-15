package main

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

type Client struct {
	name     string
	conn     *ssh.ServerConn
	channel  ssh.Channel
	messages chan string
	width    int
	height   int
}

type ChatServer struct {
	clients     map[string]*Client
	history     []string
	mutex       sync.RWMutex
	messages    chan string
	guestNumber int64
}

func NewChatServer() *ChatServer {
	return &ChatServer{
		clients:  make(map[string]*Client),
		history:  make([]string, 0),
		messages: make(chan string, 100),
	}
}

func (s *ChatServer) getGuestName() string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.guestNumber++
	return fmt.Sprintf("guest%d", s.guestNumber)
}

func (s *ChatServer) broadcast(message string) {
	s.mutex.Lock()
	s.history = append(s.history, message)
	s.mutex.Unlock()

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	for _, client := range s.clients {
		select {
		case client.messages <- message:
		default:
		}
	}
}

func (c *Client) writeLines(term *term.Terminal, lines ...string) {
	for _, line := range lines {
		// Word wrap long lines to fit terminal width
		if len(line) > c.width {
			wrapped := ""
			words := strings.Split(line, " ")
			lineLen := 0

			for _, word := range words {
				if lineLen+len(word)+1 > c.width {
					wrapped += "\n" + word + " "
					lineLen = len(word) + 1
				} else {
					wrapped += word + " "
					lineLen += len(word) + 1
				}
			}
			line = wrapped
		}
		term.Write([]byte(line + "\n"))
	}
}

func generateRandomName() string {
	adjectives := []string{"Happy", "Clever", "Brave", "Gentle", "Swift", "Bright", "Kind", "Wise"}
	nouns := []string{"Dolphin", "Eagle", "Lion", "Panda", "Tiger", "Wolf", "Bear", "Fox"}

	adjIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(adjectives))))
	nounIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(nouns))))

	return adjectives[adjIndex.Int64()] + nouns[nounIndex.Int64()]
}

func (s *ChatServer) handleClient(client *Client) {
	// Clear screen and move cursor to top
	client.channel.Write([]byte("\x1b[2J\x1b[H"))

	// Create centered banner based on terminal width
	bannerText := "Welcome to the Chat Server!"
	banner := strings.Repeat("-", client.width) + "\n"
	padding := (client.width - len(bannerText)) / 2
	if padding > 0 {
		banner += strings.Repeat(" ", padding) + bannerText + "\n"
		banner += strings.Repeat("-", client.width) + "\n"
	} else {
		banner += bannerText + "\n" + strings.Repeat("-", client.width) + "\n"
	}

	// Create terminal
	term := term.NewTerminal(client.channel, "> ")
	term.SetSize(client.height, client.width)

	// Write banner
	client.writeLines(term, banner)

	// Send chat history
	s.mutex.RLock()
	for _, msg := range s.history {
		client.writeLines(term, msg)
	}
	s.mutex.RUnlock()

	// Broadcast join message with debounce
	time.Sleep(100 * time.Millisecond) // Small delay to prevent rapid reconnect spam
	joinMsg := fmt.Sprintf("[%s] %s has joined the chat", time.Now().Format("15:04"), client.name)
	s.broadcast(joinMsg)

	// Start message sender goroutine
	done := make(chan struct{})
	messageQueue := make(chan string, 100)

	go func() {
		defer close(done)
		for msg := range messageQueue {
			// Clear the current line before printing the message
			term.Write([]byte("\r\x1b[K" + msg + "\n"))
		}
	}()

	// Forward messages to the queue
	go func() {
		for msg := range client.messages {
			messageQueue <- msg
		}
		close(messageQueue)
	}()

	// Connection stability check
	lastInput := time.Now()
	stableConnection := make(chan bool, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		stableConnection <- true
	}()

	// Handle input
	for {
		line, err := term.ReadLine()
		if err != nil {
			break
		}

		if time.Since(lastInput) < 100*time.Millisecond {
			// Ignore rapid inputs
			continue
		}
		lastInput = time.Now()

		if line == "\x1b" || line == "" {
			continue
		}

		if len(line) > 0 {
			msg := fmt.Sprintf("[%s] %s: %s", time.Now().Format("15:04"), client.name, line)
			s.broadcast(msg)
			// Clear the prompt after sending
			term.Write([]byte("\r\x1b[K> "))
		}
	}

	// Wait for connection to stabilize before broadcasting leave message
	select {
	case <-stableConnection:
		// Cleanup on disconnect
		s.mutex.Lock()
		delete(s.clients, client.name)
		s.mutex.Unlock()

		close(client.messages)
		<-done

		leaveMsg := fmt.Sprintf("[%s] %s has left the chat", time.Now().Format("15:04"), client.name)
		s.broadcast(leaveMsg)
	default:
		// Connection wasn't stable, skip leave message
	}

	client.channel.Close()
}

func main() {
	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal("Failed to generate private key:", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		log.Fatal("Failed to create signer:", err)
	}
	config.AddHostKey(signer)

	chatServer := NewChatServer()

	listener, err := net.Listen("tcp", "0.0.0.0:2222")
	if err != nil {
		log.Fatal("Failed to listen on port 2222:", err)
	}

	fmt.Println("Chat server started on port 2222")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
		if err != nil {
			log.Printf("Failed to handshake: %v", err)
			continue
		}

		go ssh.DiscardRequests(reqs)

		go func() {
			for newChannel := range chans {
				if newChannel.ChannelType() != "session" {
					newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
					continue
				}

				channel, requests, err := newChannel.Accept()
				if err != nil {
					log.Printf("Failed to accept channel: %v", err)
					continue
				}

				// Initialize client with default terminal size
				client := &Client{
					conn:     sshConn,
					channel:  channel,
					messages: make(chan string, 100),
					width:    80, // default width
					height:   24, // default height
				}

				// Get username from SSH connection or generate random name
				if sshConn.User() != "" {
					client.name = sshConn.User()
				} else {
					client.name = generateRandomName()
				}

				// Handle window size changes and other requests
				go func(in <-chan *ssh.Request) {
					for req := range in {
						ok := false
						switch req.Type {
						case "pty-req":
							ok = true
							// Parse terminal dimensions from pty-req
							if len(req.Payload) >= 8 {
								client.width = int(req.Payload[3])
								client.height = int(req.Payload[2])
							}
						case "window-change":
							ok = true
							if len(req.Payload) >= 8 {
								client.width = int(req.Payload[2])<<8 | int(req.Payload[3])
								client.height = int(req.Payload[0])<<8 | int(req.Payload[1])
							}
						case "shell":
							ok = true
						}
						if req.WantReply {
							req.Reply(ok, nil)
						}
					}
				}(requests)

				chatServer.mutex.Lock()
				chatServer.clients[client.name] = client
				chatServer.mutex.Unlock()

				go chatServer.handleClient(client)
			}
		}()
	}
}
