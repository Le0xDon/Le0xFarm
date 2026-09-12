package controllerbackup

import (
	"fmt"
	"testing"
	"time"
)

func TestRetentionKeepsNewestHourlyDailyAndAllOperatorBackups(t *testing.T) {
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	var values []Manifest
	for index := range 24 * 10 {
		id, _ := ParseBackupID(fmt.Sprintf("backup_%032x", index+1))
		values = append(values, Manifest{BackupID: id, Class: ClassHourly, CreatedAt: base.Add(-time.Duration(index) * time.Hour)})
	}
	manual, _ := ParseBackupID("backup_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	safety, _ := ParseBackupID("backup_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	values = append(values, Manifest{BackupID: manual, Class: ClassManual, CreatedAt: base.Add(-time.Hour * 999)}, Manifest{BackupID: safety, Class: ClassSafety, CreatedAt: base.Add(-time.Hour * 999)})
	kept := Retained(values, 24, 7)
	if !kept[manual] || !kept[safety] {
		t.Fatal("manual or pre-restore safety backup was pruned")
	}
	for index := range 24 {
		if !kept[values[index].BackupID] {
			t.Fatalf("newest hourly %d was not retained", index)
		}
	}
	days := map[string]bool{}
	for _, item := range values {
		if item.Class == ClassHourly && kept[item.BackupID] {
			days[item.CreatedAt.Format("2006-01-02")] = true
		}
	}
	if len(days) != 7 {
		t.Fatalf("day-separated recovery points=%d", len(days))
	}
}
