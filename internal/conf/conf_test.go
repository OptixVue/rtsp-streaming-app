package conf

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aler9/gortsplib/v2"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/nacl/secretbox"

	"github.com/aler9/rtsp-simple-server/internal/logger"
)

func writeTempFile(byts []byte) (string, error) {
	tmpf, err := os.CreateTemp(os.TempDir(), "rtsp-")
	if err != nil {
		return "", err
	}
	defer tmpf.Close()

	_, err = tmpf.Write(byts)
	if err != nil {
		return "", err
	}

	return tmpf.Name(), nil
}

func TestConfFromFile(t *testing.T) {
	func() {
		tmpf, err := writeTempFile([]byte("logLevel: debug\n" +
			"paths:\n" +
			"  cam1:\n" +
			"    runOnDemandStartTimeout: 5s\n"))
		require.NoError(t, err)
		defer os.Remove(tmpf)

		conf, hasFile, err := Load(tmpf)
		require.NoError(t, err)
		require.Equal(t, true, hasFile)

		require.Equal(t, LogLevel(logger.Debug), conf.LogLevel)

		pa, ok := conf.Paths["cam1"]
		require.Equal(t, true, ok)
		require.Equal(t, &PathConf{
			Source:                     "publisher",
			SourceOnDemandStartTimeout: 10 * StringDuration(time.Second),
			SourceOnDemandCloseAfter:   10 * StringDuration(time.Second),
			RunOnDemandStartTimeout:    5 * StringDuration(time.Second),
			RunOnDemandCloseAfter:      10 * StringDuration(time.Second),
		}, pa)
	}()

	func() {
		tmpf, err := writeTempFile([]byte(``))
		require.NoError(t, err)
		defer os.Remove(tmpf)

		_, _, err = Load(tmpf)
		require.NoError(t, err)
	}()

	func() {
		tmpf, err := writeTempFile([]byte(`paths:`))
		require.NoError(t, err)
		defer os.Remove(tmpf)

		_, _, err = Load(tmpf)
		require.NoError(t, err)
	}()

	func() {
		tmpf, err := writeTempFile([]byte(
			"paths:\n" +
				"  mypath:\n"))
		require.NoError(t, err)
		defer os.Remove(tmpf)

		_, _, err = Load(tmpf)
		require.NoError(t, err)
	}()
}

func TestConfFromFileAndEnv(t *testing.T) {
	os.Setenv("RTSP_PATHS_CAM1_SOURCE", "rtsp://testing")
	defer os.Unsetenv("RTSP_PATHS_CAM1_SOURCE")

	os.Setenv("RTSP_PROTOCOLS", "tcp")
	defer os.Unsetenv("RTSP_PROTOCOLS")

	tmpf, err := writeTempFile([]byte("{}"))
	require.NoError(t, err)
	defer os.Remove(tmpf)

	conf, hasFile, err := Load(tmpf)
	require.NoError(t, err)
	require.Equal(t, true, hasFile)

	require.Equal(t, Protocols{Protocol(gortsplib.TransportTCP): {}}, conf.Protocols)

	pa, ok := conf.Paths["cam1"]
	require.Equal(t, true, ok)
	require.Equal(t, &PathConf{
		Source:                     "rtsp://testing",
		SourceOnDemandStartTimeout: 10 * StringDuration(time.Second),
		SourceOnDemandCloseAfter:   10 * StringDuration(time.Second),
		RunOnDemandStartTimeout:    10 * StringDuration(time.Second),
		RunOnDemandCloseAfter:      10 * StringDuration(time.Second),
	}, pa)
}

func TestConfFromEnvOnly(t *testing.T) {
	os.Setenv("RTSP_PATHS_CAM1_SOURCE", "rtsp://testing")
	defer os.Unsetenv("RTSP_PATHS_CAM1_SOURCE")

	conf, hasFile, err := Load("rtsp-simple-server.yml")
	require.NoError(t, err)
	require.Equal(t, false, hasFile)

	pa, ok := conf.Paths["cam1"]
	require.Equal(t, true, ok)
	require.Equal(t, &PathConf{
		Source:                     "rtsp://testing",
		SourceOnDemandStartTimeout: 10 * StringDuration(time.Second),
		SourceOnDemandCloseAfter:   10 * StringDuration(time.Second),
		RunOnDemandStartTimeout:    10 * StringDuration(time.Second),
		RunOnDemandCloseAfter:      10 * StringDuration(time.Second),
	}, pa)
}

func TestConfEncryption(t *testing.T) {
	key := "testing123testin"
	plaintext := "paths:\n" +
		"  path1:\n" +
		"  path2:\n"

	encryptedConf := func() string {
		var secretKey [32]byte
		copy(secretKey[:], key)

		var nonce [24]byte
		if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
			panic(err)
		}

		encrypted := secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &secretKey)
		return base64.StdEncoding.EncodeToString(encrypted)
	}()

	os.Setenv("RTSP_CONFKEY", key)
	defer os.Unsetenv("RTSP_CONFKEY")

	tmpf, err := writeTempFile([]byte(encryptedConf))
	require.NoError(t, err)
	defer os.Remove(tmpf)

	conf, hasFile, err := Load(tmpf)
	require.NoError(t, err)
	require.Equal(t, true, hasFile)

	_, ok := conf.Paths["path1"]
	require.Equal(t, true, ok)

	_, ok = conf.Paths["path2"]
	require.Equal(t, true, ok)
}

func TestConfErrors(t *testing.T) {
	for _, ca := range []struct {
		name string
		conf string
		err  string
	}{
		{
			"non existent parameter 1",
			`invalid: param`,
			"non-existent parameter: 'invalid'",
		},
		{
			"non existent parameter 2",
			"paths:\n" +
				"  mypath:\n" +
				"    invalid: parameter\n",
			"parameter paths, key mypath: non-existent parameter: 'invalid'",
		},
		{
			"invalid path name",
			"paths:\n" +
				"  '':\n" +
				"    source: publisher\n",
			"invalid path name '': cannot be empty",
		},
		{
			"double raspberry pi camera",
			"paths:\n" +
				"  cam1:\n" +
				"    source: rpiCamera\n" +
				"  cam2:\n" +
				"    source: rpiCamera\n",
			"'rpiCamera' with same camera ID 0 is used as source in two paths, 'cam1' and 'cam2'",
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			tmpf, err := writeTempFile([]byte(ca.conf))
			require.NoError(t, err)
			defer os.Remove(tmpf)

			_, _, err = Load(tmpf)
			require.EqualError(t, err, ca.err)
		})
	}
}
