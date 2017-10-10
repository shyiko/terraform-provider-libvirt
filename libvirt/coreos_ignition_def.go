package libvirt

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"

	libvirt "github.com/libvirt/libvirt-go"
	"github.com/mitchellh/packer/common/uuid"
)

type defIgnition struct {
	Name     string
	PoolName string
	Content  string
}

// Creates a new cloudinit with the defaults
// the provider uses
func newIgnitionDef() defIgnition {
	ign := defIgnition{}

	return ign
}

// Create a ISO file based on the contents of the CloudInit instance and
// uploads it to the libVirt pool
// Returns a string holding terraform's internal ID of this resource
func (ign *defIgnition) CreateAndUpload(virConn *libvirt.Connect, poolSync *LibVirtPoolSync) (string, error) {
	pool, err := virConn.LookupStoragePoolByName(ign.PoolName)
	if err != nil {
		return "", fmt.Errorf("can't find storage pool '%s': %v", ign.PoolName, err)
	}
	defer pool.Free()

	lock := poolSync.GetLock(ign.PoolName)
	lock.Lock()
	defer lock.Unlock()

	// Refresh the pool of the volume so that libvirt knows it is
	// not longer in use.
	err = WaitForSuccess("Error refreshing pool for volume", func() error {
		return pool.Refresh(0)
	})
	if err != nil {
		return "", err
	}

	volumeDef := newDefVolume()
	volumeDef.Name = ign.Name

	ignFile, err := ign.createFile()
	if err != nil {
		return "", err
	}
	defer func() {
		// Remove the tmp ignition file
		if err = os.Remove(ignFile); err != nil {
			log.Printf("Error while removing tmp Ignition file: %s", err)
		}
	}()

	img, err := newImage(ignFile)
	if err != nil {
		return "", err
	}

	size, err := img.Size()
	if err != nil {
		return "", err
	}

	volumeDef.Capacity.Unit = "B"
	volumeDef.Capacity.Value = size
	volumeDef.Target.Format.Type = "raw"

	volumeDefXml, err := xml.Marshal(volumeDef)
	if err != nil {
		return "", fmt.Errorf("Error serializing libvirt volume: %s", err)
	}

	// create the volume
	volume, err := pool.StorageVolCreateXML(string(volumeDefXml), 0)
	if err != nil {
		return "", fmt.Errorf("Error creating libvirt volume for Ignition %s: %s", ign.Name, err)
	}
	defer volume.Free()

	// upload ignition file
	err = img.Import(newCopier(virConn, volume, volumeDef.Capacity.Value), volumeDef)
	if err != nil {
		return "", fmt.Errorf("Error while uploading ignition file %s: %s", img.String(), err)
	}

	key, err := volume.GetKey()
	if err != nil {
		return "", fmt.Errorf("Error retrieving volume key: %s", err)
	}

	return ign.buildTerraformKey(key), nil
}

// create a unique ID for terraform use
// The ID is made by the volume ID (the internal one used by libvirt)
// joined by the ";" with a UUID
func (ign *defIgnition) buildTerraformKey(volumeKey string) string {
	return fmt.Sprintf("%s;%s", volumeKey, uuid.TimeOrderedUUID())
}

func getIgnitionVolumeKeyFromTerraformID(id string) (string, error) {
	s := strings.SplitN(id, ";", 2)
	if len(s) != 2 {
		return "", fmt.Errorf("%s is not a valid key", id)
	}
	return s[0], nil
}

// Dumps the Ignition object - either generated by Terraform or supplied as a file -
// to a temporary ignition file
func (ign *defIgnition) createFile() (string, error) {
	log.Print("Creating Ignition temporary file")
	tempFile, err := ioutil.TempFile("", ign.Name)
	if err != nil {
		return "", fmt.Errorf("Cannot create tmp file for Ignition: %s",
			err)
	}
	defer tempFile.Close()

	var file bool
	file = true
	if _, err := os.Stat(ign.Content); err != nil {
		var js map[string]interface{}
		if err_conf := json.Unmarshal([]byte(ign.Content), &js); err_conf != nil {
			return "", fmt.Errorf("coreos_ignition 'content' is neither a file "+
				"nor a valid json object %s", ign.Content)
		}
		file = false
	}

	if !file {
		if _, err := tempFile.WriteString(ign.Content); err != nil {
			return "", fmt.Errorf("Cannot write Ignition object to temporary " +
				"ignition file")
		}
	} else if file {
		ignFile, err := os.Open(ign.Content)
		if err != nil {
			return "", fmt.Errorf("Error opening supplied Ignition file %s", ign.Content)
		}
		defer ignFile.Close()
		_, err = io.Copy(tempFile, ignFile)
		if err != nil {
			return "", fmt.Errorf("Error copying supplied Igition file to temporary file: %s", ign.Content)
		}
	}
	return tempFile.Name(), nil
}

// Creates a new defIgnition object from provided id
func newIgnitionDefFromRemoteVol(virConn *libvirt.Connect, id string) (defIgnition, error) {
	ign := defIgnition{}

	key, err := getIgnitionVolumeKeyFromTerraformID(id)
	if err != nil {
		return ign, err
	}

	volume, err := virConn.LookupStorageVolByKey(key)
	if err != nil {
		return ign, fmt.Errorf("Can't retrieve volume %s", key)
	}
	defer volume.Free()

	ign.Name, err = volume.GetName()
	if err != nil {
		return ign, fmt.Errorf("Error retrieving volume name: %s", err)
	}

	volPool, err := volume.LookupPoolByVolume()
	if err != nil {
		return ign, fmt.Errorf("Error retrieving pool for volume: %s", err)
	}
	defer volPool.Free()

	ign.PoolName, err = volPool.GetName()
	if err != nil {
		return ign, fmt.Errorf("Error retrieving pool name: %s", err)
	}

	return ign, nil
}
