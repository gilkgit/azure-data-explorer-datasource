package azuredx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/google/uuid"

	"golang.org/x/oauth2/microsoft"

	"github.com/grafana/azure-data-explorer-datasource/pkg/log"
	"github.com/grafana/grafana_plugin_model/go/datasource"
	"github.com/hashicorp/go-hclog"
	"golang.org/x/oauth2/clientcredentials"
)

// QueryModel contains the query information from the API call that we use to make a query.
type QueryModel struct {
	Format    string `json:"resultFormat"`
	QueryType string `json:"queryType"`
	Query     string `json:"query"`
	Database  string `json:"database"`
	MacroData MacroData
}

// Interpolate applys macro expansion on the QueryModel's Payload's Query string
func (qm *QueryModel) Interpolate() (err error) {
	qm.Query, err = qm.MacroData.Interpolate(qm.Query)
	return
}

// dataSourceData holds the datasource configuration information for Azure Data Explorer's API
// that is needed to execute a request against Azure's Data Explorer API.
type dataSourceData struct {
	ClientID        string `json:"clientId"`
	TenantID        string `json:"tenantId"`
	ClusterURL      string `json:"clusterUrl"`
	DefaultDatabase string `json:"defaultDatabase"`
	Secret          string `json:"-"`
}

// Client is an http.Client used for API requests.
type Client struct {
	*http.Client
	*dataSourceData
	Log hclog.Logger
}

// RequestPayload is the information that makes up a Kusto query for Azure's Data Explorer API.
type RequestPayload struct {
	DB         string `json:"db"`
	CSL        string `json:"csl"`
	Properties string `json:"properties"`
}

// newDataSourceData creates a dataSourceData from the plugin API's DatasourceInfo's
// JSONData and Encrypted JSONData which contains the information needed to connected to
// the datasource.
func newDataSourceData(dInfo *datasource.DatasourceInfo) (*dataSourceData, error) {
	d := dataSourceData{}
	err := json.Unmarshal([]byte(dInfo.GetJsonData()), &d)
	if err != nil {
		return nil, err
	}
	d.Secret = dInfo.GetDecryptedSecureJsonData()["clientSecret"]
	return &d, nil
}

// NewClient creates a new Azure Data Explorer http client from the DatasourceInfo.
// AAD OAuth authentication is setup for the client.
func NewClient(ctx context.Context, dInfo *datasource.DatasourceInfo) (*Client, error) {
	c := Client{}
	var err error
	c.dataSourceData, err = newDataSourceData(dInfo)
	if err != nil {
		return nil, err
	}

	conf := clientcredentials.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.Secret,
		TokenURL:     microsoft.AzureADEndpoint(c.TenantID).TokenURL,
		Scopes:       []string{"https://kusto.kusto.windows.net/.default"},
	}

	c.Client = conf.Client(ctx)
	return &c, nil
}

// TestRequest handles a data source test request in Grafana's Datasource configuration UI.
func (c *Client) TestRequest() error {
	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(RequestPayload{
		CSL: ".show databases schema",
		DB:  c.DefaultDatabase,
	})
	if err != nil {
		return err
	}

	resp, err := c.Post(c.ClusterURL+"/v1/rest/query", "application/json", &buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		return fmt.Errorf("HTTP error: %v", resp.Status)
	}
	return nil
}

// KustoRequest executes a Kusto Query language request to Azure's Data Explorer V1 REST API
// and returns a TableResponse. If there is a query syntax error, the error message inside
// the API's JSON error response is returned as well (if available).
func (c *Client) KustoRequest(payload RequestPayload) (*TableResponse, string, error) {
	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(payload)
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequest(http.MethodPost, c.ClusterURL+"/v1/rest/query", &buf)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-ms-app", "Grafana-ADX")
	req.Header.Set("x-ms-client-request-id", "KGC.execute;"+uuid.Must(uuid.NewRandom()).String())
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, "", err
		}
		bodyString := string(bodyBytes)
		if resp.StatusCode == 401 { // 401 does not have a JSON body
			return nil, "", fmt.Errorf("HTTP error: %v - %v", resp.Status, bodyString)
		}
		errorData := &errorResponse{}
		err = json.Unmarshal(bodyBytes, errorData)
		if err != nil {
			log.Print.Debug("failed to unmarshal error body from response: %v", err)
		}
		return nil, errorData.Error.Message, fmt.Errorf("HTTP error: %v - %v", resp.Status, bodyString)
	}
	tr, err := tableFromJSON(resp.Body)
	return tr, "", err
}

func tableFromJSON(rc io.Reader) (*TableResponse, error) {
	tr := &TableResponse{}
	decoder := json.NewDecoder(rc)
	// Numbers as string (json.Number) so we can keep types as best we can (since the response has 'type' of column)
	decoder.UseNumber()
	err := decoder.Decode(tr)
	if err != nil {
		return nil, err
	}
	if tr.Tables == nil || len(tr.Tables) == 0 {
		return nil, fmt.Errorf("unable to parse response, parsed response has no tables")
	}
	return tr, nil
}

// errorResponse is a minimal structure of Azure Data Explorer's JSON
// error body,
type errorResponse struct {
	Error struct {
		Message string `json:"@message"`
	} `json:"error"`
}
