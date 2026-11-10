package api

import "testing"

func TestAllocateConservesEveryRupiah(t *testing.T) {
	a := allocate(101, []int64{1, 1, 1})
	if a[0]+a[1]+a[2] != 101 || a[0] != 34 || a[1] != 34 || a[2] != 33 {
		t.Fatalf("allocation %v", a)
	}
}
func TestAllocateZeroWeightRetained(t *testing.T) {
	a := allocate(100, []int64{0, 0})
	if a[0] != 0 || a[1] != 0 {
		t.Fatal(a)
	}
}
func TestAllocateLargeInteger(t *testing.T) {
	a := allocate(1000000000000, []int64{1000000000000, 2000000000000})
	if a[0]+a[1] != 1000000000000 {
		t.Fatal(a)
	}
}
func TestCSVFormulaNeutralized(t *testing.T) {
	for _, s := range []string{"=1+1", "+SUM(A1)", "-1+1", "@SUM(A1)", "\t=1", "  =1"} {
		if csvSafe(s)[0] != '\'' {
			t.Fatal(s)
		}
	}
	if csvSafe("Normal") != "Normal" {
		t.Fatal("normal changed")
	}
}

func TestAllocateWeightedConservation(t *testing.T) {
	for total := int64(1); total < 101; total++ {
		for n := 1; n < 20; n++ {
			weights := make([]int64, n)
			for i := range weights {
				weights[i] = int64(i + 1)
			}
			allocated := allocate(total, weights)
			var sum int64
			for _, amount := range allocated {
				if amount < 0 {
					t.Fatal("negative allocation")
				}
				sum += amount
			}
			if sum != total {
				t.Fatalf("total=%d weights=%v allocations=%v", total, weights, allocated)
			}
		}
	}
}
