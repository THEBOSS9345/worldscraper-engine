package crawl

import "log"

// logf is the crawler's single logging entry point. Engine output is captured
// by the desktop shell, so everything goes to the standard logger.
func logf(format string, args ...any) {
	log.Printf("[crawl] "+format, args...)
}
