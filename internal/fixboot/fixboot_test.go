package fixboot

import "testing"

func TestFindEFIPart(t *testing.T) {
	cases := []struct {
		in, disk, part string
	}{
		{"/dev/sda1", "/dev/sda", "1"},
		{"/dev/sda12", "/dev/sda", "12"},
		{"/dev/nvme0n1p1", "/dev/nvme0n1", "1"},
		{"/dev/nvme1n1p3", "/dev/nvme1n1", "3"},
		{"/dev/mmcblk0p2", "/dev/mmcblk0", "2"},
		// ESP on a different disk than the clone target must keep its own
		// disk name instead of being mangled into a bogus part number.
		{"/dev/sdb1", "/dev/sdb", "1"},
	}
	for _, c := range cases {
		disk, part := findEFIPart(c.in)
		if disk != c.disk || part != c.part {
			t.Errorf("findEFIPart(%q) = (%q, %q), want (%q, %q)", c.in, disk, part, c.disk, c.part)
		}
	}
}
