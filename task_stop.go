package main

func validTaskStopToken(token string) bool {
	if len(token) != 32 {
		return false
	}
	for _, digit := range token {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
}
