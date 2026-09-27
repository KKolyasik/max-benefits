package knowledge

import (
	"reflect"
	"testing"
)

// Every card of the real base survives a trip to a message and back.
func TestCardContract(t *testing.T) {
	cards, _ := realBase(t)
	for _, c := range cards {
		if back := CardFromContract(c.Contract()); !reflect.DeepEqual(back, c) {
			t.Errorf("card %s changed:\n got %+v\nwant %+v", c.ID, back, c)
		}
	}
}
