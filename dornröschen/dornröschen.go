// dornröschen wakes up machines and NASen and backs up data/syncs NAS contents.
package main

import (
	"flag"
	"fmt"
	"github.com/stapelberg/zkj-nas-tools/ping"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	runBackup = flag.Bool("backup",
		true,
		"Backup all -backup_hosts? See also -sync")
	runSync = flag.Bool("sync",
		true,
		"Sync all -storage_hosts? See also -backup")
	backupHosts = flag.String("backup_hosts",
		"midna/38:60:77:ab:d3:ea,x200/00:1f:16:1a:f5:b8",
		"Comma-separated list of hosts to back up, each entry is host/mac-address")
	storageHosts = flag.String("storage_hosts",
		"10.0.0.250/00:08:9b:d0:31:ef,10.0.0.251/00:08:9b:d1:6f:39",
		"Comma-separated list of NASen, each entry is host/mac-address")
	backupPrivateKeyPath = flag.String("ssh_backup_private_key_path",
		"/root/.ssh/id_rsa_backup",
		"Path to the SSH private key file to authenticate with at -backup_hosts for backing up")
	suspendPrivateKeyPath = flag.String("ssh_suspend_private_key_path",
		"/root/.ssh/id_rsa_suspend",
		"Path to the SSH private key file to authenticate with at -backup_hosts for suspending to RAM")
	syncPrivateKeyPath = flag.String("ssh_sync_private_key_path",
		"/root/.ssh/id_rsa_sync",
		"Path to the SSH private key file to authenticate with at -storage_hosts for syncing")
)

func splitHostMAC(hostmac string) (host, mac string) {
	parts := strings.Split(hostmac, "/")
	if len(parts) != 2 {
		log.Fatalf(`"%s" is not in format host/MAC`, hostmac)
	}
	return parts[0], parts[1]
}

func wakeUp(host, mac string) (bool, error) {
	result := make(chan *time.Duration)
	go ping.Ping(host, 5*time.Second, result)
	if <-result != nil {
		log.Printf("Host %s responding to pings, not waking up.\n", host)
		return false, nil
	}

	// Parse MAC address
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		log.Fatalf(`MAC address "%s" does not consist of 6 parts`, mac)
	}
	macParts := make([]uint8, 6)
	for idx, str := range parts {
		converted, err := strconv.ParseUint(str, 16, 8)
		if err != nil {
			log.Fatalf("Invalid MAC address part: %s: %v\n", str, err)
		}
		macParts[idx] = uint8(converted)
	}

	// Send magic Wake-On-LAN packet
	payload := make([]byte, 102)
	for idx := 0; idx < 6; idx++ {
		payload[idx] = 0xff
	}
	for n := 0; n < 16; n++ {
		for part := 0; part < 6; part++ {
			payload[6+(n*6)+part] = macParts[part]
		}
	}
	socket, err := net.DialUDP("udp4", nil, &net.UDPAddr{
		IP: net.IPv4(255, 255, 255, 255),
		// udp/9 is the discard protocol
		Port: 9,
	})
	if err != nil {
		log.Fatalf("Cannot open UDP broadcast socket: %v\n", err)
	}
	socket.Write(payload)
	socket.Close()
	log.Printf("Sent magic packet to %02x:%02x:%02x:%02x:%02x:%02x\n",
		macParts[0], macParts[1], macParts[2], macParts[3], macParts[4], macParts[5])

	timeout := 120 * time.Second
	packetSent := time.Now()
	for time.Since(packetSent) < timeout {
		go ping.Ping(host, 1*time.Second, result)
		if <-result != nil {
			log.Printf("Host %s woke up after waiting %v.\n", host, time.Since(packetSent))
			return true, nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return true, fmt.Errorf("Host %s not responding to pings within %v after sending magic packet", host, timeout)
}

func dramaqueenRequest(NAS, lock, method string) {
	retry := 0
	for retry < 5 {
		retry++
		resp, err := http.Post("http://"+NAS+":4414/"+method+"?key="+lock, "text/plain", nil)
		if err != nil {
			if retry == 5 {
				log.Fatalf(`Could not acquire dramaqueen lock on %s: %v`, NAS, err)
			} else {
				log.Printf(`Could not acquire dramaqueen lock on %s: %v`, NAS, err)
				time.Sleep(time.Duration(math.Pow(2, float64(retry))) * time.Second)
				continue
			}
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			log.Fatalf(`dramaqueen request on %s resulted in HTTP %d`, NAS, resp.StatusCode)
		}
		break
	}
}

func lockDramaqueen(NAS, lock string) {
	// TODO: dramaqueen should return an error if the lock already exists so that overruns will fail.
	dramaqueenRequest(NAS, lock, "inhibit")
}

func releaseDramaqueenLock(NAS, lock string) {
	dramaqueenRequest(NAS, lock, "release")
}

func backup(NASen []string) {
	// Alternate between the available NASen to make sure each one works.
	dest := NASen[(time.Now().Day()+1)%len(NASen)]
	log.Printf("Backup destination is %s", dest)
	destHost, destMAC := splitHostMAC(dest)

	if _, err := wakeUp(destHost, destMAC); err != nil {
		log.Fatalf("Could not wake up NAS %s\n", destHost)
	}

	for _, source := range strings.Split(*backupHosts, ",") {
		sourceHost, sourceMAC := splitHostMAC(source)

		// Prevent dramaqueen on the destination NAS from shutting it down.
		lockname := "backup-" + sourceHost
		lockDramaqueen(destHost, lockname)
		defer releaseDramaqueenLock(destHost, lockname)

		woken, err := wakeUp(sourceHost, sourceMAC)
		if err != nil {
			log.Printf("Backup of %s failed: %v\n", sourceHost, err)
			continue
		}

		// The command is just destHost, because for the SSH key this program
		// is using, the remote host will only ever run /root/backup.pl, which
		// interprets the command as the destination host.
		output, err := sshCommand(sourceHost, *backupPrivateKeyPath, destHost)
		if err != nil {
			log.Printf("Backup of %s failed: %v\n", sourceHost, err)
			continue
		}
		log.Printf("backup command yielded %s\n", output)

		// Suspend the machine to RAM, but only if we have woken it up.
		if !woken {
			continue
		}

		if _, err := sshCommand(sourceHost, *suspendPrivateKeyPath, ""); err != nil {
			log.Printf("Suspending %s to RAM failed: %v\n", sourceHost, err)
		}
	}
}

func sync(NASen []string) {
	for _, dest := range NASen {
		destHost, destMAC := splitHostMAC(dest)
		if _, err := wakeUp(destHost, destMAC); err != nil {
			log.Fatalf("Could not wake up NAS %s\n", destHost)
		}
		lockDramaqueen(destHost, "sync")
	}

	for idx, source := range NASen {
		dest := NASen[(idx+1)%len(NASen)]
		sourceHost, _ := splitHostMAC(source)
		destHost, _ := splitHostMAC(dest)
		log.Printf("Syncing %s to %s\n", sourceHost, destHost)

		output, err := sshCommand(sourceHost, *syncPrivateKeyPath, destHost)
		if err != nil {
			log.Printf("Syncing of %s to %s failed: %v\n", sourceHost, destHost, err)
		}
		log.Printf("sync %s to %s output: %s\n", sourceHost, destHost, output)
	}

	for _, dest := range NASen {
		destHost, _ := splitHostMAC(dest)
		// With the lock released, the NASen will turn off on their own (unless
		// somebody is using them, of course).
		releaseDramaqueenLock(destHost, "sync")
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

	if !*runBackup && !*runSync {
		log.Fatal("Neither -backup nor -sync enabled, nothing to do.")
	}

	storageList := strings.Split(*storageHosts, ",")
	if len(storageList) > 2 {
		log.Fatal("More than 2 -storage_hosts are not supported. Please send a patch to fix.")
	}

	if *runBackup {
		backup(storageList)
	}

	if *runSync {
		sync(storageList)
	}
}
