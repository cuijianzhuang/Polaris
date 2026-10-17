package panel

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func ginTestContext(w http.ResponseWriter, r *http.Request) (*gin.Context, *gin.Engine) {
	c, e := gin.CreateTestContext(w)
	c.Request = r
	return c, e
}
