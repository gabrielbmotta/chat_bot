# Chat Bot

Based almost entirely off of sample code provided in the WhatsMeow repo, this is currently the skeleton of a WhatsApp bot. To implement any reactions to standard WhatsApp events, simply add code to the `handler` function, under the relevant event type.

## Sending messages

WhatsApp uses a unique identifitfier for each user and group, called a JID. This ID looks similar to an email address. If you have a string containing a JID, you can convert it into a JID obejct with the `parseJID` function, and can send a message to that JID with the `sendMessage` function.

As an example, if we wanted to send a message to someone with JID 1234567890@s.whatsapp.net, we could do something like this:

```go
destJID, _ := parseJID("1234567890@s.whatsapp.net")
sendMessage(destJID, "Hello!")
```
