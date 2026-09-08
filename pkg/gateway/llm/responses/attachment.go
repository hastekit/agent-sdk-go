package responses

import "fmt"

// UndeliverableAttachment is what a provider translator puts in place of an
// attachment it cannot carry.
//
// The alternative is to drop it, which is what the translators used to do, and
// dropping is the worst of the options: the request goes through, the model
// answers a question about a picture it was never shown, and nothing anywhere
// says an attachment went missing. A line of text in its place costs a few
// tokens and makes the failure something the model can say out loud.
//
// It never reaches history. Translators run when a request is built, on the
// native form; this text exists only in what goes to the provider.
func UndeliverableAttachment(kind, reason string) string {
	return fmt.Sprintf("[The user attached %s, but it could not be sent to this model: %s.]", kind, reason)
}
