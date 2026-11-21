// Package roles is the permission hierarchy used for command authorization. The
// numeric values are public (dashboard and plugins): never change them.
package roles

type Role int

const (
	User       Role = 0
	GroupAdmin Role = 1
	GroupOwner Role = 2
	BotAdmin   Role = 3
)

func (r Role) String() string {
	switch r {
	case User:
		return "user"
	case GroupAdmin:
		return "group_admin"
	case GroupOwner:
		return "group_owner"
	case BotAdmin:
		return "bot_admin"
	default:
		return "unknown"
	}
}

func FromOneBotRole(role string) Role {
	switch role {
	case "admin":
		return GroupAdmin
	case "owner":
		return GroupOwner
	default:
		return User
	}
}

func Max(a, b Role) Role {
	if a > b {
		return a
	}
	return b
}

func Parse(value int) Role {
	switch {
	case value <= 0:
		return User
	case value == 1:
		return GroupAdmin
	case value == 2:
		return GroupOwner
	default:
		return BotAdmin
	}
}
