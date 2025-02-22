// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main

import (
	"crypto/rand"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/canonical/go-efilib"
	"github.com/canonical/go-tpm2"
	"github.com/canonical/tcglog-parser"
	"github.com/snapcore/snapd/asserts"

	"golang.org/x/xerrors"

	"github.com/snapcore/secboot"
	secboot_efi "github.com/snapcore/secboot/efi"
	"github.com/snapcore/secboot/internal/testutil"
	secboot_tpm2 "github.com/snapcore/secboot/tpm2"
)

var (
	outputDir string
)

type mockEFIEnvironment struct {
	efivars string
	log     string
}

func (e *mockEFIEnvironment) ReadVar(name string, guid efi.GUID) ([]byte, efi.VariableAttributes, error) {
	return testutil.EFIReadVar(e.efivars, name, guid)
}

func (e *mockEFIEnvironment) ReadEventLog() (*tcglog.Log, error) {
	f, err := os.Open(e.log)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return tcglog.ReadLog(f, &tcglog.LogOptions{})
}

func init() {
	flag.StringVar(&outputDir, "output", "", "Specify the output directory")
}

func computePCRProtectionProfile(env secboot_efi.HostEnvironment) (*secboot_tpm2.PCRProtectionProfile, error) {
	profile := secboot_tpm2.NewPCRProtectionProfile()

	sbpParams := secboot_efi.SecureBootPolicyProfileParams{
		PCRAlgorithm: tpm2.HashAlgorithmSHA256,
		LoadSequences: []*secboot_efi.ImageLoadEvent{
			{
				Source: secboot_efi.Firmware,
				Image:  secboot_efi.FileImage("efi/testdata/mockshim1.efi.signed.1"),
				Next: []*secboot_efi.ImageLoadEvent{
					{
						Source: secboot_efi.Shim,
						Image:  secboot_efi.FileImage("efi/testdata/mockgrub1.efi.signed.shim"),
						Next: []*secboot_efi.ImageLoadEvent{
							{
								Source: secboot_efi.Shim,
								Image:  secboot_efi.FileImage("efi/testdata/mockkernel1.efi.signed.shim"),
							},
						},
					},
				},
			},
		},
		Environment: env,
	}

	if err := secboot_efi.AddSecureBootPolicyProfile(profile, &sbpParams); err != nil {
		return nil, xerrors.Errorf("cannot add secureboot policy profile: %w", err)
	}

	sdefisParams := secboot_efi.SystemdStubProfileParams{
		PCRAlgorithm: tpm2.HashAlgorithmSHA256,
		PCRIndex:     12,
		KernelCmdlines: []string{
			"snapd_recovery_mode=run quiet console=tty1 panic=-1",
			"snapd_recovery_mode=recover quiet console=tty1 panic=-1",
		},
	}

	if err := secboot_efi.AddSystemdStubProfile(profile, &sdefisParams); err != nil {
		return nil, xerrors.Errorf("cannot add systemd EFI stub profile: %w", err)
	}

	modelData, err := ioutil.ReadFile("tools/gen-compattest-data/data/fake-model")
	if err != nil {
		return nil, xerrors.Errorf("cannot read model assertion: %w", err)
	}

	model, err := asserts.Decode(modelData)
	if err != nil {
		return nil, xerrors.Errorf("cannot decode model assertion: %w", err)
	}

	smParams := secboot_tpm2.SnapModelProfileParams{
		PCRAlgorithm: tpm2.HashAlgorithmSHA256,
		PCRIndex:     12,
		Models:       []secboot.SnapModel{model.(secboot.SnapModel)},
	}

	if err := secboot_tpm2.AddSnapModelProfile(profile, &smParams); err != nil {
		return nil, xerrors.Errorf("cannot add snap model profile: %w", err)
	}

	return profile, nil
}

func run() int {
	if outputDir != "" {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Cannot create output directory: %v\n", err)
			return 1
		}
	}

	cleanupTpmSimulator, err := testutil.LaunchTPMSimulator(&testutil.TPMSimulatorOptions{SourceDir: outputDir, Manufacture: true, SavePersistent: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot launch TPM simulator: %v\n", err)
		return 1
	}
	defer cleanupTpmSimulator()

	restore := testutil.MockOpenDefaultTctiFn(func() (tpm2.TCTI, error) {
		return tpm2.OpenMssim("", testutil.MssimPort, testutil.MssimPort+1)
	})
	defer restore()

	env := &mockEFIEnvironment{"efi/testdata/efivars2", "efi/testdata/eventlog1.bin"}

	tpm, err := secboot_tpm2.ConnectToDefaultTPM()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open TPM simulator connection: %v\n", err)
		return 1
	}
	defer tpm.Close()

	caCertRaw, caKey, err := testutil.CreateTestCA()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create test CA certificate: %v\n", err)
		return 1
	}

	ekCert, err := testutil.CreateTestEKCert(tpm.TPMContext, caCertRaw, caKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create test EK certificate: %v\n", err)
		return 1
	}

	if err := testutil.CertifyTPM(tpm.TPMContext, ekCert); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot certify TPM: %v\n", err)
		return 1
	}

	caCert, err := x509.ParseCertificate(caCertRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot parse test CA certificate: %v\n", err)
		return 1
	}

	if err := secboot_tpm2.SaveEKCertificateChain(nil, []*x509.Certificate{caCert}, filepath.Join(outputDir, "EKCertData")); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot save EK certificate chain: %v\n", err)
		return 1
	}

	if err := tpm.EnsureProvisioned(secboot_tpm2.ProvisionModeFull, []byte("1234")); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot provision TPM: %v\n", err)
		return 1
	}

	pcrProfile, err := computePCRProtectionProfile(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot compute PCR profile: %v\n", err)
		return 1
	}

	key := make([]byte, 64)
	rand.Read(key)

	params := secboot_tpm2.KeyCreationParams{
		PCRProfile:             pcrProfile,
		PCRPolicyCounterHandle: 0x01801000,
	}

	keyFile := filepath.Join(outputDir, "key")
	os.Remove(keyFile)

	authKey, err := secboot_tpm2.SealKeyToTPM(tpm, key, keyFile, &params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot seal key: %v\n", err)
		return 1
	}

	if err := ioutil.WriteFile(filepath.Join(outputDir, "clearKey"), key, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot write cleartext key: %v\n", err)
		return 1
	}

	if err := ioutil.WriteFile(filepath.Join(outputDir, "authKey"), authKey, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot write policy update auth key: %v\n", err)
		return 1
	}

	// Write out PCR event sequences corresponding to the generated profile.
	// The form is 'PCR Alg Digest'
	// XXX(chrisccoulson): It would be nice to implement a way to autogenerate these
	pcrEvents1 := `7 11 ccfc4bb32888a345bc8aeadaba552b627d99348c767681ab3141f5b01e40a40e
7 11 9af72c68c7de19603879020c14f88c2bfa8d06503153866b9888c48d0c5d2a58
7 11 b56d4033d9002a59221d3776ab2557fd4ce17c5367943716669118734be66319
7 11 700e8fb6c9772fad3333dc0e8a654fdde7485de844940cced27c80881cbc3fff
7 11 1963d580fcc0cede165e23837b55335eebe18750c0b795883386026ea071e3c6
7 11 df3f619804a92fdb4057192dc43dd748ea778adc52bc498ce80524c014b81119
7 11 ef6179fc571480150176c28cdea83156d83e44897464c483149a945f5160800e
7 11 6f39dc51f71a13c734c69cb783a3563ceb5f2da7f6dec1ca1018308b8d9f500e
12 11 94ae5f11b45bbf919fd1bf52db3e625fb576d21af7150f9bb36b7fe65834ef1a
12 11 df3f619804a92fdb4057192dc43dd748ea778adc52bc498ce80524c014b81119
12 11 d64df514d7ac57c1a28c5f2a3abc39340d9b7fe3f76cc3acc991d418f095d5b0
`
	pcrEvents2 := `7 11 ccfc4bb32888a345bc8aeadaba552b627d99348c767681ab3141f5b01e40a40e
7 11 9af72c68c7de19603879020c14f88c2bfa8d06503153866b9888c48d0c5d2a58
7 11 b56d4033d9002a59221d3776ab2557fd4ce17c5367943716669118734be66319
7 11 700e8fb6c9772fad3333dc0e8a654fdde7485de844940cced27c80881cbc3fff
7 11 1963d580fcc0cede165e23837b55335eebe18750c0b795883386026ea071e3c6
7 11 df3f619804a92fdb4057192dc43dd748ea778adc52bc498ce80524c014b81119
7 11 ef6179fc571480150176c28cdea83156d83e44897464c483149a945f5160800e
7 11 6f39dc51f71a13c734c69cb783a3563ceb5f2da7f6dec1ca1018308b8d9f500e
12 11 7598387669ac1cbad0ea568d9675d8e3a71870a53554bbbe92a6f4d9a8133944
12 11 df3f619804a92fdb4057192dc43dd748ea778adc52bc498ce80524c014b81119
12 11 d64df514d7ac57c1a28c5f2a3abc39340d9b7fe3f76cc3acc991d418f095d5b0
`

	for i, seq := range []string{pcrEvents1, pcrEvents2} {
		if err := ioutil.WriteFile(filepath.Join(outputDir, fmt.Sprintf("pcrSequence.%d", i+1)), []byte(seq), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Cannot write PCR event sequence: %v\n", err)
			return 1
		}
	}

	return 0
}

func main() {
	flag.Parse()
	os.Exit(run())
}
