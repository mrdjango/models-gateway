package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func ImportTensorGridCatalog(c *gin.Context) {
	var request model.TensorGridCatalogImport
	if err := c.ShouldBindJSON(&request); err != nil {
		tensorGridError(c, err)
		return
	}
	result, created, err := model.ImportTensorGridCatalog(request)
	if err != nil {
		if errors.Is(err, model.ErrTensorGridCatalogAlreadyImported) {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"code":    "catalog_already_imported",
				"message": err.Error(),
			})
			return
		}
		tensorGridError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "created": created, "data": result})
}
