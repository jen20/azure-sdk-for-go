package azure

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"time"
	"unicode"
)

const (
	azureRoleURL           = "services/hostedservices/%s/deployments/%s/roles/%s"
	azureOperationsURL     = "services/hostedservices/%s/deployments/%s/roleinstances/%s/Operations"
	azureCertificatListURL = "services/hostedservices/%s/certificates"
	azureRoleSizeListURL   = "rolesizes"

	osLinux                   = "Linux"
	osWindows                 = "Windows"
	dockerPublicConfigVersion = 2

	provisioningConfDoesNotExistsError = "You should set azure VM provisioning config first"
	invalidCertExtensionError          = "Certificate %s is invalid. Please specify %s certificate."
	invalidOSError                     = "You must specify correct OS param. Valid values are 'Linux' and 'Windows'"
	invalidPasswordLengthError         = "Password must be between 4 and 30 characters."
	invalidPasswordError               = "Password must have at least one upper case, lower case and numeric character."
	invalidRoleSizeError               = "Invalid role size: %s. Available role sizes: %s"
	invalidRoleSizeInLocationError     = "Role size: %s not available in location: %s."
)

type VmClient struct {
	client *Client
}

func (client *Client) Vm() *VmClient {
	return &VmClient{client: client}
}

//Region public methods starts

func (self *VmClient) CreateAzureVM(azureVMConfiguration *Role, dnsName, location string) error {
	if azureVMConfiguration == nil {
		return fmt.Errorf(paramNotSpecifiedError, "azureVMConfiguration")
	}
	if len(dnsName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "dnsName")
	}
	if len(location) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "location")
	}

	err := self.verifyDNSname(dnsName)
	if err != nil {
		return err
	}

	hostedServiceClient := self.client.HostedService()

	requestId, err := hostedServiceClient.CreateHostedService(dnsName, location, "")
	if err != nil {
		return err
	}

	self.client.waitAsyncOperation(requestId)

	if azureVMConfiguration.UseCertAuth {
		err = self.uploadServiceCert(dnsName, azureVMConfiguration.CertPath)
		if err != nil {
			hostedServiceClient.DeleteHostedService(dnsName)
			return err
		}
	}

	vMDeployment := self.createVMDeploymentConfig(azureVMConfiguration)
	vMDeploymentBytes, err := xml.Marshal(vMDeployment)
	if err != nil {
		hostedServiceClient.DeleteHostedService(dnsName)
		return err
	}

	requestURL := fmt.Sprintf(azureDeploymentListURL, azureVMConfiguration.RoleName)
	requestId, err = self.client.sendAzurePostRequest(requestURL, vMDeploymentBytes)
	if err != nil {
		hostedServiceClient.DeleteHostedService(dnsName)
		return err
	}

	self.client.waitAsyncOperation(requestId)

	return nil
}

func (self *VmClient) CreateAzureVMConfiguration(dnsName, instanceSize, imageName, location string) (*Role, error) {
	if len(dnsName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "dnsName")
	}
	if len(instanceSize) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "instanceSize")
	}
	if len(imageName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "imageName")
	}
	if len(location) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "location")
	}

	err := self.verifyDNSname(dnsName)
	if err != nil {
		return nil, err
	}

	locationInfo, err := self.client.Location().GetLocation(location)
	if err != nil {
		return nil, err
	}

	sizeAvailable, err := self.isInstanceSizeAvailableInLocation(locationInfo, instanceSize)
	if err != nil {
		return nil, err
	}

	if sizeAvailable == false {
		return nil, fmt.Errorf(invalidRoleSizeInLocationError, instanceSize, location)
	}

	role, err := self.createAzureVMRole(dnsName, instanceSize, imageName, location)
	if err != nil {
		return nil, err
	}

	return role, nil
}

func (self *VmClient) AddAzureLinuxProvisioningConfig(azureVMConfiguration *Role, userName, password, certPath string, sshPort int) (*Role, error) {
	if azureVMConfiguration == nil {
		return nil, fmt.Errorf(paramNotSpecifiedError, "azureVMConfiguration")
	}
	if len(userName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "userName")
	}

	configurationSets := ConfigurationSets{}
	provisioningConfig, err := self.createLinuxProvisioningConfig(azureVMConfiguration.RoleName, userName, password, certPath)
	if err != nil {
		return nil, err
	}

	configurationSets.ConfigurationSet = append(configurationSets.ConfigurationSet, provisioningConfig)

	networkConfig, networkErr := self.createNetworkConfig(osLinux, sshPort)
	if networkErr != nil {
		return nil, err
	}

	configurationSets.ConfigurationSet = append(configurationSets.ConfigurationSet, networkConfig)

	azureVMConfiguration.ConfigurationSets = configurationSets

	if len(certPath) > 0 {
		azureVMConfiguration.UseCertAuth = true
		azureVMConfiguration.CertPath = certPath
	}

	return azureVMConfiguration, nil
}

func (self *VmClient) SetAzureVMExtension(azureVMConfiguration *Role, name string, publisher string, version string, referenceName string, state string, publicConfigurationValue string, privateConfigurationValue string) (*Role, error) {
	if azureVMConfiguration == nil {
		return nil, fmt.Errorf(paramNotSpecifiedError, "azureVMConfiguration")
	}
	if len(name) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "name")
	}
	if len(publisher) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "publisher")
	}
	if len(version) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "version")
	}
	if len(referenceName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "referenceName")
	}

	extension := ResourceExtensionReference{}
	extension.Name = name
	extension.Publisher = publisher
	extension.Version = version
	extension.ReferenceName = referenceName
	extension.State = state

	if len(privateConfigurationValue) > 0 {
		privateConfig := ResourceExtensionParameter{}
		privateConfig.Key = "ignored"
		privateConfig.Value = base64.StdEncoding.EncodeToString([]byte(privateConfigurationValue))
		privateConfig.Type = "Private"

		extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue = append(extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue, privateConfig)
	}

	if len(publicConfigurationValue) > 0 {
		publicConfig := ResourceExtensionParameter{}
		publicConfig.Key = "ignored"
		publicConfig.Value = base64.StdEncoding.EncodeToString([]byte(publicConfigurationValue))
		publicConfig.Type = "Public"

		extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue = append(extension.ResourceExtensionParameterValues.ResourceExtensionParameterValue, publicConfig)
	}

	azureVMConfiguration.ResourceExtensionReferences.ResourceExtensionReference = append(azureVMConfiguration.ResourceExtensionReferences.ResourceExtensionReference, extension)

	return azureVMConfiguration, nil
}

func (self *VmClient) SetAzureDockerVMExtension(azureVMConfiguration *Role, dockerPort int, version string) (*Role, error) {
	if azureVMConfiguration == nil {
		return nil, fmt.Errorf(paramNotSpecifiedError, "azureVMConfiguration")
	}

	if len(version) == 0 {
		version = "0.3"
	}

	err := self.addDockerPort(azureVMConfiguration.ConfigurationSets.ConfigurationSet, dockerPort)
	if err != nil {
		return nil, err
	}

	publicConfiguration, err := self.createDockerPublicConfig(dockerPort)
	if err != nil {
		return nil, err
	}

	privateConfiguration := "{}"

	azureVMConfiguration, err = self.SetAzureVMExtension(azureVMConfiguration, "DockerExtension", "MSOpenTech.Extensions", version, "DockerExtension", "enable", publicConfiguration, privateConfiguration)
	return azureVMConfiguration, nil
}

func (self *VmClient) GetVMDeployment(cloudserviceName, deploymentName string) (*VMDeployment, error) {
	if len(cloudserviceName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "cloudserviceName")
	}
	if len(deploymentName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "deploymentName")
	}

	deployment := new(VMDeployment)

	requestURL := fmt.Sprintf(azureDeploymentURL, cloudserviceName, deploymentName)
	response, azureErr := self.client.sendAzureGetRequest(requestURL)
	if azureErr != nil {
		return nil, azureErr
	}

	err := xml.Unmarshal(response, deployment)
	if err != nil {
		return nil, err
	}

	return deployment, nil
}

func (self *VmClient) DeleteVMDeployment(cloudserviceName, deploymentName string) error {
	if len(cloudserviceName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "cloudserviceName")
	}
	if len(deploymentName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "deploymentName")
	}

	requestURL := fmt.Sprintf(deleteAzureDeploymentURL, cloudserviceName, deploymentName)
	requestId, err := self.client.sendAzureDeleteRequest(requestURL)
	if err != nil {
		return err
	}

	self.client.waitAsyncOperation(requestId)

	return nil
}

func (self *VmClient) GetRole(cloudserviceName, deploymentName, roleName string) (*Role, error) {
	if len(cloudserviceName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "cloudserviceName")
	}
	if len(deploymentName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "deploymentName")
	}
	if len(roleName) == 0 {
		return nil, fmt.Errorf(paramNotSpecifiedError, "roleName")
	}

	role := new(Role)

	requestURL := fmt.Sprintf(azureRoleURL, cloudserviceName, deploymentName, roleName)
	response, azureErr := self.client.sendAzureGetRequest(requestURL)
	if azureErr != nil {
		return nil, azureErr
	}

	err := xml.Unmarshal(response, role)
	if err != nil {
		return nil, err
	}

	return role, nil
}

func (self *VmClient) StartRole(cloudserviceName, deploymentName, roleName string) error {
	if len(cloudserviceName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "cloudserviceName")
	}
	if len(deploymentName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "deploymentName")
	}
	if len(roleName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "roleName")
	}

	startRoleOperation := self.createStartRoleOperation()

	startRoleOperationBytes, err := xml.Marshal(startRoleOperation)
	if err != nil {
		return err
	}

	requestURL := fmt.Sprintf(azureOperationsURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := self.client.sendAzurePostRequest(requestURL, startRoleOperationBytes)
	if azureErr != nil {
		return azureErr
	}

	self.client.waitAsyncOperation(requestId)
	return nil
}

func (self *VmClient) ShutdownRole(cloudserviceName, deploymentName, roleName string) error {
	if len(cloudserviceName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "cloudserviceName")
	}
	if len(deploymentName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "deploymentName")
	}
	if len(roleName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "roleName")
	}

	shutdownRoleOperation := self.createShutdowRoleOperation()

	shutdownRoleOperationBytes, err := xml.Marshal(shutdownRoleOperation)
	if err != nil {
		return err
	}

	requestURL := fmt.Sprintf(azureOperationsURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := self.client.sendAzurePostRequest(requestURL, shutdownRoleOperationBytes)
	if azureErr != nil {
		return azureErr
	}

	self.client.waitAsyncOperation(requestId)
	return nil
}

func (self *VmClient) RestartRole(cloudserviceName, deploymentName, roleName string) error {
	if len(cloudserviceName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "cloudserviceName")
	}
	if len(deploymentName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "deploymentName")
	}
	if len(roleName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "roleName")
	}

	restartRoleOperation := self.createRestartRoleOperation()

	restartRoleOperationBytes, err := xml.Marshal(restartRoleOperation)
	if err != nil {
		return err
	}

	requestURL := fmt.Sprintf(azureOperationsURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := self.client.sendAzurePostRequest(requestURL, restartRoleOperationBytes)
	if azureErr != nil {
		return azureErr
	}

	self.client.waitAsyncOperation(requestId)
	return nil
}

func (self *VmClient) DeleteRole(cloudserviceName, deploymentName, roleName string) error {
	if len(cloudserviceName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "cloudserviceName")
	}
	if len(deploymentName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "deploymentName")
	}
	if len(roleName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "roleName")
	}

	requestURL := fmt.Sprintf(azureRoleURL, cloudserviceName, deploymentName, roleName)
	requestId, azureErr := self.client.sendAzureDeleteRequest(requestURL)
	if azureErr != nil {
		return azureErr
	}

	self.client.waitAsyncOperation(requestId)
	return nil
}

func (self *VmClient) GetRoleSizeList() (RoleSizeList, error) {
	roleSizeList := RoleSizeList{}

	response, err := self.client.sendAzureGetRequest(azureRoleSizeListURL)
	if err != nil {
		return roleSizeList, err
	}

	err = xml.Unmarshal(response, &roleSizeList)
	if err != nil {
		return roleSizeList, err
	}

	return roleSizeList, err
}

func (self *VmClient) ResolveRoleSize(roleSizeName string) error {
	if len(roleSizeName) == 0 {
		return fmt.Errorf(paramNotSpecifiedError, "roleSizeName")
	}

	roleSizeList, err := self.GetRoleSizeList()
	if err != nil {
		return err
	}

	for _, roleSize := range roleSizeList.RoleSizes {
		if roleSize.Name != roleSizeName {
			continue
		}

		return nil
	}

	var availableSizes bytes.Buffer
	for _, existingSize := range roleSizeList.RoleSizes {
		availableSizes.WriteString(existingSize.Name + ", ")
	}

	return errors.New(fmt.Sprintf(invalidRoleSizeError, roleSizeName, strings.Trim(availableSizes.String(), ", ")))
}

//Region public methods ends

//Region private methods starts

func (self *VmClient) createStartRoleOperation() StartRoleOperation {
	startRoleOperation := StartRoleOperation{}
	startRoleOperation.OperationType = "StartRoleOperation"
	startRoleOperation.Xmlns = azureXmlns

	return startRoleOperation
}

func (self *VmClient) createShutdowRoleOperation() ShutdownRoleOperation {
	shutdownRoleOperation := ShutdownRoleOperation{}
	shutdownRoleOperation.OperationType = "ShutdownRoleOperation"
	shutdownRoleOperation.Xmlns = azureXmlns

	return shutdownRoleOperation
}

func (self *VmClient) createRestartRoleOperation() RestartRoleOperation {
	startRoleOperation := RestartRoleOperation{}
	startRoleOperation.OperationType = "RestartRoleOperation"
	startRoleOperation.Xmlns = azureXmlns

	return startRoleOperation
}

func (self *VmClient) createDockerPublicConfig(dockerPort int) (string, error) {
	config := dockerPublicConfig{DockerPort: dockerPort, Version: dockerPublicConfigVersion}
	configJson, err := json.Marshal(config)
	if err != nil {
		return "", err
	}

	return string(configJson), nil
}

func (self *VmClient) addDockerPort(configurationSets []ConfigurationSet, dockerPort int) error {
	if len(configurationSets) == 0 {
		return errors.New(provisioningConfDoesNotExistsError)
	}

	for i := 0; i < len(configurationSets); i++ {
		if configurationSets[i].ConfigurationSetType != "NetworkConfiguration" {
			continue
		}

		dockerEndpoint := self.createEndpoint("docker", "tcp", dockerPort, dockerPort)
		configurationSets[i].InputEndpoints.InputEndpoint = append(configurationSets[i].InputEndpoints.InputEndpoint, dockerEndpoint)
	}

	return nil
}

func (self *VmClient) createVMDeploymentConfig(role *Role) VMDeployment {
	deployment := VMDeployment{}
	deployment.Name = role.RoleName
	deployment.Xmlns = azureXmlns
	deployment.DeploymentSlot = "Production"
	deployment.Label = role.RoleName
	deployment.RoleList.Role = append(deployment.RoleList.Role, role)

	return deployment
}

func (self *VmClient) createAzureVMRole(name, instanceSize, imageName, location string) (*Role, error) {
	config := new(Role)
	config.RoleName = name
	config.RoleSize = instanceSize
	config.RoleType = "PersistentVMRole"
	config.ProvisionGuestAgent = true
	var err error
	config.OSVirtualHardDisk, err = self.createOSVirtualHardDisk(name, imageName, location)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func (self *VmClient) createOSVirtualHardDisk(dnsName, imageName, location string) (OSVirtualHardDisk, error) {
	oSVirtualHardDisk := OSVirtualHardDisk{}

	err := self.client.Image().ResolveImageName(imageName)
	if err != nil {
		return oSVirtualHardDisk, err
	}

	oSVirtualHardDisk.SourceImageName = imageName
	oSVirtualHardDisk.MediaLink, err = self.getVHDMediaLink(dnsName, location)
	if err != nil {
		return oSVirtualHardDisk, err
	}

	return oSVirtualHardDisk, nil
}

func (self *VmClient) getVHDMediaLink(dnsName, location string) (string, error) {
	storageServiceClient := self.client.StorageService()

	storageService, err := storageServiceClient.GetStorageServiceByLocation(location)
	if err != nil {
		return "", err
	}

	if storageService == nil {

		uuid, err := newUUID()
		if err != nil {
			return "", err
		}

		serviceName := "portalvhds" + uuid
		storageService, err = storageServiceClient.CreateStorageService(serviceName, location)
		if err != nil {
			return "", err
		}
	}

	blobEndpoint, err := storageServiceClient.GetBlobEndpoint(storageService)
	if err != nil {
		return "", err
	}

	vhdMediaLink := blobEndpoint + "vhds/" + dnsName + "-" + time.Now().Local().Format("20060102150405") + ".vhd"
	return vhdMediaLink, nil
}

func (self *VmClient) createLinuxProvisioningConfig(dnsName, userName, userPassword, certPath string) (ConfigurationSet, error) {
	provisioningConfig := ConfigurationSet{}

	disableSshPasswordAuthentication := false
	if len(userPassword) == 0 {
		disableSshPasswordAuthentication = true
		// We need to set dummy password otherwise azure API will throw an error
		userPassword = "P@ssword1"
	} else {
		err := self.verifyPassword(userPassword)
		if err != nil {
			return provisioningConfig, err
		}
	}

	provisioningConfig.DisableSshPasswordAuthentication = disableSshPasswordAuthentication
	provisioningConfig.ConfigurationSetType = "LinuxProvisioningConfiguration"
	provisioningConfig.HostName = dnsName
	provisioningConfig.UserName = userName
	provisioningConfig.UserPassword = userPassword

	if len(certPath) > 0 {
		var err error
		provisioningConfig.SSH, err = self.createSshConfig(certPath, userName)
		if err != nil {
			return provisioningConfig, err
		}
	}

	return provisioningConfig, nil
}

func (self *VmClient) uploadServiceCert(dnsName, certPath string) error {
	certificateConfig, err := self.createServiceCertDeploymentConf(certPath)
	if err != nil {
		return err
	}

	certificateConfigBytes, err := xml.Marshal(certificateConfig)
	if err != nil {
		return err
	}

	requestURL := fmt.Sprintf(azureCertificatListURL, dnsName)
	requestId, azureErr := self.client.sendAzurePostRequest(requestURL, certificateConfigBytes)
	if azureErr != nil {
		return azureErr
	}

	err = self.client.waitAsyncOperation(requestId)
	return err
}

func (self *VmClient) createServiceCertDeploymentConf(certPath string) (ServiceCertificate, error) {
	certConfig := ServiceCertificate{}
	certConfig.Xmlns = azureXmlns
	data, err := ioutil.ReadFile(certPath)
	if err != nil {
		return certConfig, err
	}

	certData := base64.StdEncoding.EncodeToString(data)
	certConfig.Data = certData
	certConfig.CertificateFormat = "pfx"

	return certConfig, nil
}

func (self *VmClient) createSshConfig(certPath, userName string) (SSH, error) {
	sshConfig := SSH{}
	publicKey := PublicKey{}

	err := self.checkServiceCertExtension(certPath)
	if err != nil {
		return sshConfig, err
	}

	fingerprint, err := self.getServiceCertFingerprint(certPath)
	if err != nil {
		return sshConfig, err
	}

	publicKey.Fingerprint = fingerprint
	publicKey.Path = "/home/" + userName + "/.ssh/authorized_keys"

	sshConfig.PublicKeys.PublicKey = append(sshConfig.PublicKeys.PublicKey, publicKey)
	return sshConfig, nil
}

func (self *VmClient) getServiceCertFingerprint(certPath string) (string, error) {
	certData, readErr := ioutil.ReadFile(certPath)
	if readErr != nil {
		return "", readErr
	}

	block, rest := pem.Decode(certData)
	if block == nil {
		return "", errors.New(string(rest))
	}

	sha1sum := sha1.Sum(block.Bytes)
	fingerprint := fmt.Sprintf("%X", sha1sum)
	return fingerprint, nil
}

func (self *VmClient) checkServiceCertExtension(certPath string) error {
	certParts := strings.Split(certPath, ".")
	certExt := certParts[len(certParts)-1]

	acceptedExtension := "pem"
	if certExt != acceptedExtension {
		return errors.New(fmt.Sprintf(invalidCertExtensionError, certPath, acceptedExtension))
	}

	return nil
}

func (self *VmClient) createNetworkConfig(os string, sshPort int) (ConfigurationSet, error) {
	networkConfig := ConfigurationSet{}
	networkConfig.ConfigurationSetType = "NetworkConfiguration"

	var endpoint InputEndpoint
	if os == osLinux {
		endpoint = self.createEndpoint("ssh", "tcp", sshPort, 22)
	} else if os == osWindows {
		//!TODO add rdp endpoint
	} else {
		return networkConfig, errors.New(fmt.Sprintf(invalidOSError))
	}

	networkConfig.InputEndpoints.InputEndpoint = append(networkConfig.InputEndpoints.InputEndpoint, endpoint)

	return networkConfig, nil
}

func (self *VmClient) createEndpoint(name string, protocol string, extertalPort int, internalPort int) InputEndpoint {
	endpoint := InputEndpoint{}
	endpoint.Name = name
	endpoint.Protocol = protocol
	endpoint.Port = extertalPort
	endpoint.LocalPort = internalPort

	return endpoint
}

func (self *VmClient) verifyDNSname(dns string) error {
	if len(dns) < 3 || len(dns) > 25 {
		return fmt.Errorf(invalidDnsLengthError)
	}

	return nil
}

func (self *VmClient) verifyPassword(password string) error {
	if len(password) < 4 || len(password) > 30 {
		return fmt.Errorf(invalidPasswordLengthError)
	}

next:
	for _, classes := range map[string][]*unicode.RangeTable{
		"upper case": {unicode.Upper, unicode.Title},
		"lower case": {unicode.Lower},
		"numeric":    {unicode.Number, unicode.Digit},
	} {
		for _, r := range password {
			if unicode.IsOneOf(classes, r) {
				continue next
			}
		}
		return fmt.Errorf(invalidPasswordError)
	}
	return nil
}

func (self *VmClient) isInstanceSizeAvailableInLocation(location *Location, instanceSize string) (bool, error) {
	if len(instanceSize) == 0 {
		return false, fmt.Errorf(paramNotSpecifiedError, "vmSize")
	}

	for _, availableRoleSize := range location.VirtualMachineRoleSizes {
		if availableRoleSize == instanceSize {
			return true, nil
		}
	}

	return false, nil
}

//Region private methods ends