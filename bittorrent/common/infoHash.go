package common

type InfoHash string

func (ih *InfoHash) Valid() bool {
	return len(*ih) == 20
}
