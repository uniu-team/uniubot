// Package all registers every built-in adapter. Importing it is the single place
// that decides which adapters are compiled in.
package all

import (
	_ "github.com/uniu-network/uniubot/internal/adapters/onebothttp"
	_ "github.com/uniu-network/uniubot/internal/adapters/webchat"
)
