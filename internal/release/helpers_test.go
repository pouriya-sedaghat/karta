package release

import "github.com/pouriya-sedaghat/karta/internal/dbconn"

func dbParamsForTest() dbconn.Params {
	return dbconn.Params{Host: "localhost", Port: 5432, User: "karta_api", PasswordFile: "/nonexistent", SSLMode: "disable"}
}
