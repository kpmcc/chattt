package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type client chan<- string // an outgoing message channel
type clientStream chan client
type ircChan chan clientMessage

const (
	VERSION  = "1.0.0"
	layoutUS = "January 2, 2006"
)

var (
	port             = flag.String("p", "8000", "access port number")
	operatorPassword = flag.String("o", "pw", "operator password")
	entering         = make(clientStream)
	leaving          = make(clientStream)
	messages         = make(chan clientMessage) // all incoming client messages
	chanRequests     = make(chan chanRequest)
	timeCreated      = time.Now().Format(layoutUS)
)

type IRCConn struct {
	User        string
	Nick        string
	Conn        net.Conn
	RealName    string
	Welcomed    bool
	AwayMessage string
	isAway      bool
	isOperator  bool
	isDeleted   bool
	//inChannels  []*IRCChan
}

type channelControlVal int8
type serverControlVal int8

const (
	JOIN channelControlVal = iota
	PART
	OP
	TOPIC
	VOICE
)

const (
	CHANREQ serverControlVal = iota
	CHANRESP
)

type channelControl struct {
	ctrl   channelControlVal // basically an enum
	sender client            //
	params []string
}

type serverControl struct {
	srvCtrl serverControlVal
	sender  client
	params  []string
}

type IRCMessage struct {
	Prefix  string
	Command string
	Params  []string
}

type clientMessage struct {
	cli client
	msg string
	nc  net.Conn
}

type chanRequest struct {
	cli            client
	msg            string
	responseStream chan ircChan
}

func extractMessage(rawMsg []byte) (IRCMessage, error) {
	var im IRCMessage
	msgStr := string(rawMsg)
	prefix := ""
	if msgStr[0] == ':' {
		// Message has a prefix
		splitMsg := strings.SplitN(msgStr, " ", 2)
		prefix = splitMsg[0]
		msgStr = splitMsg[1]
	}
	// Split off trailing if present
	msgParts := strings.Split(msgStr, ":")
	trailing := ""
	msgStr = msgParts[0]
	if len(msgParts) == 2 {
		// msg has `trailing` param
		trailing = msgParts[1]
	} else if len(msgParts) != 1 {
		// More than one instance of ':' in command + params
		// this should not occur and indicates a malformed message
		return im, fmt.Errorf("in extractMessage, multiple colons found")
	}
	commandAndParams := strings.Split(strings.Trim(msgStr, " "), " ")
	command := commandAndParams[0]
	var params = make([]string, 0, 15)
	if len(commandAndParams) != 1 {
		for _, v := range commandAndParams[1:] {
			if v != "" && v != " " {
				params = append(params, v)
			}
		}
	}
	if trailing != "" {
		params = append(params, trailing)
	}
	im = IRCMessage{
		Prefix:  prefix,
		Command: command,
		Params:  params,
	}
	return im, nil
}

func clientWriter(conn net.Conn, ch <-chan string) {
	for msg := range ch {
		//fmt.Fprintln(conn, msg) // NOTE: ignoring network errors
		conn.Write([]byte(msg))
	}
}

func channelHandler(ch chan clientMessage, ctrlStream chan channelControl, chanName string) {
	thisChannelName := chanName
	//operators := make(map[string]bool)
	//voiceActivated := make(map[string]bool)
	//members := make(map[string]client)
	//
	for {
		select {
		case ctrl := <-ctrlStream:
			fmt.Printf("%v\n", ctrl)

		case cm := <-messages:
			msg := cm.msg
			im, _ := extractMessage([]byte(msg))
			switch im.Command {
			case "JOIN":
				fmt.Printf("%s got join\n", thisChannelName)
			case "PRIVMSG":
				fmt.Printf("%s got PRIVMSG\n", thisChannelName)
			}

		}
	}
	//for cm := range ch {

	//}
}

func handleConn(conn net.Conn) {
	ch := make(chan string) // outgoing client messages
	go clientWriter(conn, ch)

	//who := conn.RemoteAddr().String()
	//ch <- "You are " + who
	entering <- ch
	channels := make(map[string]ircChan)

	input := bufio.NewScanner(conn)
	for input.Scan() {
		incoming_message := input.Text()
		if len(incoming_message) >= 510 {
			incoming_message = incoming_message[:510]
		}
		cm := clientMessage{cli: ch, msg: incoming_message, nc: conn}
		im, _ := extractMessage([]byte(incoming_message))
		switch im.Command {
		case "JOIN":
			rs := make(chan ircChan)
			cr := chanRequest{cli: ch, msg: incoming_message, responseStream: rs}
			chanRequests <- cr
			ircCh := <-rs
			chanName := im.Params[0]
			fmt.Printf("adding channel %s to connection channel map\n", chanName)
			channels[chanName] = ircCh
			close(rs)
			ircCh <- cm
		case "PRIVMSG":
			target := im.Params[0]
			ircCh, targetIsChannel := channels[target]
			fmt.Printf("ircCh: %v targetIsChannel: %v\n", ircCh, targetIsChannel)
			if targetIsChannel {
				ircCh <- cm
			} else {
				messages <- cm
			}
		default:
			messages <- cm
		}
	}
	// NOTE: ignoring potential errors from input.Err()

	leaving <- ch
	conn.Close()
}

func sendWelcome(ic *IRCConn, cli client) {
	welcomeMsg := fmt.Sprintf(
		":%s 001 %s :Welcome to the Internet Relay Network %s!%s@%s\r\n",
		ic.Conn.LocalAddr(), ic.Nick, ic.Nick, ic.User, ic.Conn.RemoteAddr().String())

	yourHost := fmt.Sprintf(
		":%s 002 %s :Your host is %s, running version %s\r\n",
		ic.Conn.LocalAddr(), ic.Nick, ic.Conn.LocalAddr(), VERSION)

	created := fmt.Sprintf(":%s 003 %s :This server was created %s\r\n",
		ic.Conn.LocalAddr(), ic.Nick, timeCreated)

	myInfo := fmt.Sprintf(":%s 004 %s %s %s %s %s\r\n",
		ic.Conn.LocalAddr(), ic.Nick, ic.Conn.LocalAddr(), VERSION, "ao", "mtov")

	cli <- welcomeMsg
	cli <- yourHost
	cli <- created
	cli <- myInfo
}

func sendLUsers(ic *IRCConn, cli client, clients *map[client]*IRCConn) {
	numServers, numServices, numOperators, numChannels := 1, 0, 0, 0
	numUsers, numUnknownConnections, numClients := 0, 0, 0

	for _, conn := range *clients {
		if conn.Welcomed {
			numUsers++
			numClients++
		} else if conn.Nick != "*" || conn.User != "" {
			numClients++
		} else {
			numUnknownConnections++
		}
	}

	a := fmt.Sprintf(":%s 251 %s :There are %d users and %d services on %d servers\r\n",
		ic.Conn.LocalAddr(), ic.Nick, numUsers, numServices, numServers)
	b := fmt.Sprintf(":%s 252 %s %d :operator(s) online\r\n",
		ic.Conn.LocalAddr(), ic.Nick, numOperators)
	c := fmt.Sprintf(":%s 253 %s %d :unknown connection(s)\r\n",
		ic.Conn.LocalAddr(), ic.Nick, numUnknownConnections)
	d := fmt.Sprintf(":%s 254 %s %d :channels formed\r\n",
		ic.Conn.LocalAddr(), ic.Nick, numChannels)
	e := fmt.Sprintf(":%s 255 %s :I have %d clients and %d servers\r\n",
		ic.Conn.LocalAddr(), ic.Nick, numClients, numServers)
	cli <- a
	cli <- b
	cli <- c
	cli <- d
	cli <- e
}

func sendMOTD(ic *IRCConn, cli client) {

	noMOTD := fmt.Sprintf(":%s 422 %s :MOTD File is missing\r\n",
		ic.Conn.LocalAddr().String(), ic.Nick)
	cli <- noMOTD

	//motdIntro := fmt.Sprintf(":%s 375 %s :- %s Message of the day - \r\n",
	//	ic.Conn.LocalAddr(), ic.Nick, ic.Conn.LocalAddr())
	//motdLine := fmt.Sprintf(":%s 372 %s :- %s\r\n",
	//	ic.Conn.LocalAddr(), ic.Nick, "It's all brassica.")
	//endMOTD := fmt.Sprintf(":%s 376 %s :End of MOTD command\r\n",
	//	ic.Conn.LocalAddr(), ic.Nick)

	//cli <- motdIntro
	//cli <- motdLine
	//cli <- endMOTD

}

func broadcaster() {
	clients := make(map[client]*IRCConn) // all connected clients
	nicks := make(map[string]client)     // all registered nicks
	channels := make(map[string]chan clientMessage)
	for {
		select {
		case clientMsg := <-messages:
			cli := clientMsg.cli
			msg := clientMsg.msg
			nc := clientMsg.nc
			im, err := extractMessage([]byte(msg))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error extracting message\n")
			}
			ic, ok := clients[cli]
			if !ok {
				fmt.Printf("Could not retrieve IRCConn for client channel\n")
			} else {
				if ic.Conn != nil {
					if ic.Conn != nc {
						fmt.Fprintf(os.Stderr, "error connection mismatch")
					}
				} else {
					ic.Conn = nc
				}
			}
			fmt.Printf("handling message %v from connection with nick:%s user:%s\n", im, ic.Nick, ic.User)
			switch im.Command {
			case "NICK":
				nick := im.Params[0]
				_, ok := nicks[nick]
				if ok {
					// Nick in use
					fmt.Printf("Nick in use: %s\n", nick)
				} else {
					ic.Nick = nick
					nicks[nick] = cli
					fmt.Printf("Setting nick to: %s, still unregistered\n", nick)

					if ic.User != "" && ic.Nick != "" {
						fmt.Printf("registering\n")
						//rpl := fmt.Sprintf(":%s 001 %s :Welcome to the Internet Relay Network %s!%s@%s\r\n",
						//	ic.Conn.LocalAddr().String(), ic.Nick, ic.Nick, ic.User, ic.Conn.RemoteAddr().String())
						cli = nicks[ic.Nick]
						//cli <- rpl

						sendWelcome(ic, cli)
						sendLUsers(ic, cli, &clients)
						sendMOTD(ic, cli)
						ic.Welcomed = true
					} else {
						fmt.Printf("user: %s nick:%s\n", ic.User, ic.Nick)
					}
				}
			case "USER":
				user := im.Params[0]
				fmt.Printf("received user: %s\n", user)
				if ic.Welcomed {
					fmt.Println("Trying to reregister")
				} else {
					ic.User = user
					fmt.Printf("Setting user: %s\n", user)
					if ic.User != "" && ic.Nick != "" {
						fmt.Printf("registering\n")
						cli = nicks[ic.Nick]
						//rpl := fmt.Sprintf(":%s 001 %s :Welcome to the Internet Relay Network %s!%s@%s\r\n",
						//	ic.Conn.LocalAddr().String(), ic.Nick, ic.Nick, ic.User, ic.Conn.RemoteAddr().String())
						//cli <- rpl
						sendWelcome(ic, cli)
						sendLUsers(ic, cli, &clients)
						sendMOTD(ic, cli)

						ic.Welcomed = true
					} else {
						fmt.Printf("user: %s nick: %s\n", ic.User, ic.Nick)
					}
				}
			case "PRIVMSG":
				target := im.Params[0]
				message := im.Params[1]
				targetCli, tgtOk := nicks[target]
				senderIC := clients[cli]
				rpl := ""
				if !tgtOk {
					rpl = fmt.Sprintf(":%s!%s@%s 401 %s :No such nick/channel\r\n",
						senderIC.Nick, senderIC.User, senderIC.Conn.RemoteAddr(),
						target)
					cli <- rpl
					// NOSUCHNICK
				} else {
					rpl = fmt.Sprintf(":%s!%s@%s PRIVMSG %s :%s\r\n",
						senderIC.Nick, senderIC.User, senderIC.Conn.RemoteAddr(),
						target, message)
					targetCli <- rpl
				}
			}
		case chReq := <-chanRequests:
			im, _ := extractMessage([]byte(chReq.msg))
			chanName := im.Params[0]
			ircChan, ok := channels[chanName]
			if !ok {
				// make new channel
				newChan := make(chan clientMessage)
				newCtrlChan := make(chan channelControl)
				// TODO Maybe need to wait for channelHandler to start up?
				go channelHandler(newChan, newCtrlChan, chanName)
				channels[chanName] = newChan
				ircChan = newChan
				newCtrl := channelControl{ctrl: JOIN, sender: chReq.cli, params: []string{"#rc"}}
				newCtrlChan <- newCtrl
			}

			chReq.responseStream <- ircChan
		case cli := <-entering:
			ic := &IRCConn{Nick: "", User: ""}
			clients[cli] = ic
		case cli := <-leaving:
			delete(clients, cli)
			close(cli)
		}
	}
}

func main() {
	flag.Parse()
	lh := "localhost:"
	listenAddr := lh + *port
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}

	go broadcaster()
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go handleConn(conn)
	}
}
