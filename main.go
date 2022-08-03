// Based off of md_test code with the following notice:
//
// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var cli *whatsmeow.Client
var log waLog.Logger

var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var dbAddress = flag.String("db-address", "file:chatbot.db?_foreign_keys=on", "Database address")
var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")

var logLevel = "INFO"

var mut_message sync.Mutex

func main() {
	waBinary.IndentXML = true
	ParseCmdLineFlags()

	log = waLog.Stdout("Main", logLevel, true)

	dbLog := waLog.Stdout("Database", logLevel, true)
	storeContainer, err := sqlstore.New(*dbDialect, *dbAddress, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to database: %v", err)
		return
	}
	device, err := storeContainer.GetFirstDevice()
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	cli = whatsmeow.NewClient(device, waLog.Stdout("Client", logLevel, true))

	HandleFirstConnection(cli)

	cli.AddEventHandler(handler)
	err = cli.Connect()
	if err != nil {
		log.Errorf("Failed to connect: %v", err)
		return
	}

	c := make(chan os.Signal)
	input := make(chan string)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer close(input)
		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			line := strings.TrimSpace(scan.Text())
			if len(line) > 0 {
				input <- line
			}
		}
	}()

	for {
		select {
		case <-c:
			log.Infof("Interrupt received, exiting")
			cli.Disconnect()
			return
		}
	}
}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
			log.Errorf("Invalid JID %s: %v", arg, err)
			return recipient, false
		} else if recipient.User == "" {
			log.Errorf("Invalid JID %s: no server specified", arg)
			return recipient, false
		}
		return recipient, true
	}
}

var historySyncID int32
var startupTime = time.Now().Unix()

func handler(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		if len(cli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := cli.SendPresence(types.PresenceAvailable)
			if err != nil {
				log.Warnf("Failed to send available presence: %v", err)
			} else {
				log.Infof("Marked self as available")
			}
		}
	case *events.Connected, *events.PushNameSetting:
		if len(cli.Store.PushName) == 0 {
			return
		}
		// Send presence available when connecting and when the pushname is changed.
		// This makes sure that outgoing messages always have the right pushname.
		err := cli.SendPresence(types.PresenceAvailable)
		if err != nil {
			log.Warnf("Failed to send available presence: %v", err)
		} else {
			log.Infof("Marked self as available")
		}
	case *events.StreamReplaced:
		os.Exit(0)
	case *events.Message:
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		if evt.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", evt.Info.Type))
		}
		if evt.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", evt.Info.Category))
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "ephemeral")
		}

		log.Infof("Received message %s from %s (%s): %+v", evt.Info.ID, evt.Info.SourceString(), strings.Join(metaParts, ", "), evt.Message)
		log.Infof("Message received: %s", evt.Message.GetConversation())

		img := evt.Message.GetImageMessage()
		if img != nil {
			data, err := cli.Download(img)
			if err != nil {
				log.Errorf("Failed to download image: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(img.GetMimetype())
			path := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			err = os.WriteFile(path, data, 0600)
			if err != nil {
				log.Errorf("Failed to save image: %v", err)
				return
			}
			log.Infof("Saved image in message to %s", path)
		}
	case *events.Receipt:
		if evt.Type == events.ReceiptTypeRead || evt.Type == events.ReceiptTypeReadSelf {
			log.Infof("%v was read by %s at %s", evt.MessageIDs, evt.SourceString(), evt.Timestamp)
		} else if evt.Type == events.ReceiptTypeDelivered {
			log.Infof("%s was delivered to %s at %s", evt.MessageIDs[0], evt.SourceString(), evt.Timestamp)
		}
	case *events.Presence:
		if evt.Unavailable {
			if evt.LastSeen.IsZero() {
				log.Infof("%s is now offline", evt.From)
			} else {
				log.Infof("%s is now offline (last seen: %s)", evt.From, evt.LastSeen)
			}
		} else {
			log.Infof("%s is now online", evt.From)
		}
	case *events.HistorySync:
		id := atomic.AddInt32(&historySyncID, 1)
		fileName := fmt.Sprintf("history-%d-%d.json", startupTime, id)
		file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			log.Errorf("Failed to open file to write history sync: %v", err)
			return
		}
		enc := json.NewEncoder(file)
		enc.SetIndent("", "  ")
		err = enc.Encode(evt.Data)
		if err != nil {
			log.Errorf("Failed to write history sync: %v", err)
			return
		}
		log.Infof("Wrote history sync to %s", fileName)
		_ = file.Close()
	case *events.AppState:
		log.Debugf("App state event: %+v / %+v", evt.Index, evt.SyncActionValue)
	case *events.KeepAliveTimeout:
		log.Debugf("Keepalive timeout event: %+v", evt)
		if evt.ErrorCount > 3 {
			log.Debugf("Got >3 keepalive timeouts, forcing reconnect")
			go func() {
				cli.Disconnect()
				err := cli.Connect()
				if err != nil {
					log.Errorf("Error force-reconnecting after keepalive timeouts: %v", err)
				}
			}()
		}
	case *events.KeepAliveRestored:
		log.Debugf("Keepalive restored")
	}
}

func ParseCmdLineFlags() {
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
	}
}

func HandleFirstConnection(cli *whatsmeow.Client) {
	ch, err := cli.GetQRChannel(context.Background())
	if err != nil {
		// This error means that we're already logged in, so ignore it.
		if !errors.Is(err, whatsmeow.ErrQRStoreContainsID) {
			log.Errorf("Failed to get QR channel: %v", err)
		}
	} else {
		go func() {
			for evt := range ch {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				} else {
					log.Infof("QR channel result: %s", evt.Event)
				}
			}
		}()
	}
}

func sendMessage(recipient types.JID, message string) {
	mut_message.Lock()

	msg := &waProto.Message{Conversation: proto.String(message)}
	resp, err := cli.SendMessage(context.Background(), recipient, "", msg)
	if err != nil {
		log.Errorf("Error sending message: %v", err)
	} else {
		log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
	}

	mut_message.Unlock()
}

func getMessagRecipient(evt *events.Message) (types.JID, bool) {
	sender := evt.Info.SourceString()
	dest := strings.Fields(sender)
	// Get group JID if message is sent in group chat.
	return parseJID(dest[len(dest)-1])
}
