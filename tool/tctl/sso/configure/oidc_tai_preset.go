/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package configure

import (
	"context"
	"log/slog"

	"github.com/gravitational/teleport/api/types"
)

// presetTAI is the name of the TAI unified authentication preset. It is also
// the default connector name.
//
// Example:
//
//	tctl sso configure oidc --preset tai -r 'user_name,*,access' \
//	  --secret TAI_SECRET --id TAI_CLIENT_ID --issuer-url https://tai.example.com
const presetTAI = "tai"

// taiPreset configures a connector for TAI, which puts the username in the
// non-standard "user_name" claim. The issuer is deployment specific and must
// be set with --issuer-url.
var taiPreset = oidcPreset{
	name:        presetTAI,
	description: "TAI unified authentication",
	display:     "TAI",
	modifySpec: func(ctx context.Context, logger *slog.Logger, spec *types.OIDCConnectorSpecV3) error {
		if spec.UsernameClaim == "" {
			spec.UsernameClaim = "user_name"
		}
		if len(spec.Scope) == 0 {
			spec.Scope = []string{"openid", "profile"}
		}
		return nil
	},
}

func init() {
	oidcPresets = append(oidcPresets, taiPreset)
}
