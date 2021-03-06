// Copyright 2015 mokey Authors. All rights reserved.
// Use of this source code is governed by a BSD style
// license that can be found in the LICENSE file.

package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/justinas/nosurf"
	"github.com/spf13/viper"
	"github.com/ubccr/goipa"
	"github.com/ubccr/mokey/app"
	"github.com/ubccr/mokey/model"
)

func setupAccount(ctx *app.AppContext, questions []*model.SecurityQuestion, token *model.Token, r *http.Request) (*ipa.OTPToken, error) {
	pass := r.FormValue("password")
	pass2 := r.FormValue("password2")

	if len(pass) < viper.GetInt("min_passwd_len") || len(pass2) < viper.GetInt("min_passwd_len") {
		return nil, errors.New(fmt.Sprintf("Please set a password at least %d characters in length.", viper.GetInt("min_passwd_len")))
	}

	if pass != pass2 {
		return nil, errors.New("Password do not match. Please confirm your password.")
	}

	err := updateSecurityQuestion(ctx, questions, token.UserName, r)
	if err != nil {
		return nil, err
	}

	// Setup password in FreeIPA. Assumed user has no OTP tokens.
	err = setPassword(token.UserName, pass, "")
	if err != nil {
		if ierr, ok := err.(*ipa.ErrPasswordPolicy); ok {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": ierr.Error(),
			}).Error("password does not conform to policy")
			return nil, errors.New("Your password is too weak. Please ensure your password includes a number and lower/upper case character")
		}

		if ierr, ok := err.(*ipa.ErrInvalidPassword); ok {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": ierr.Error(),
			}).Error("invalid password from FreeIPA")
			return nil, errors.New("Invalid password.")
		}

		log.WithFields(log.Fields{
			"uid":   token.UserName,
			"error": err.Error(),
		}).Error("failed to set user password in FreeIPA")
		return nil, errors.New("Fatal system error")
	}

	// Create new TOTP token if required
	otptoken, err := setTOTP(token.UserName, pass)
	if err != nil {
		log.WithFields(log.Fields{
			"uid":   token.UserName,
			"error": err.Error(),
		}).Error("failed to create TOTP")
		return nil, errors.New("Fatal system error")
	}

	// Destroy token
	err = model.DestroyToken(ctx.DB, token.Token)
	if err != nil {
		log.WithFields(log.Fields{
			"uid":   token.UserName,
			"error": err.Error(),
		}).Error("failed to remove token from database")
		return nil, errors.New("Fatal system error")
	}

	return otptoken, nil
}

func SetupAccountHandler(ctx *app.AppContext) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tk, ok := model.VerifyToken(app.AccountSetupSalt, mux.Vars(r)["token"])
		if !ok {
			ctx.RenderNotFound(w)
			return
		}

		token, err := model.FetchToken(ctx.DB, tk, viper.GetInt("setup_max_age"))
		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Failed to fetch token from database")
			ctx.RenderNotFound(w)
			return
		}

		if token.Attempts > viper.GetInt("max_attempts") {
			log.WithFields(log.Fields{
				"token": token.Token,
				"uid":   token.UserName,
			}).Error("Too many attempts for token.")
			ctx.RenderNotFound(w)
			return
		}

		questions, err := model.FetchQuestions(ctx.DB)
		if err != nil {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": err.Error(),
			}).Error("Failed to fetch questions from database")
			ctx.RenderError(w, http.StatusInternalServerError)
			return
		}

		client := app.NewIpaClient(true)
		userRec, err := client.UserShow(token.UserName)
		if err != nil {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": err.Error(),
			}).Error("Failed to fetch user record from freeipa")
			ctx.RenderError(w, http.StatusInternalServerError)
			return
		}

		message := ""
		completed := false
		var otptoken *ipa.OTPToken
		otpdata := ""

		if r.Method == "POST" {
			var err error
			otptoken, err = setupAccount(ctx, questions, token, r)
			if err != nil {
				message = err.Error()
				completed = false

				err := model.IncrementToken(ctx.DB, token.Token)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err.Error(),
					}).Error("Failed to increment token attempts")
				}
			} else {
				completed = true
				err = ctx.SendEmail(token.Email, fmt.Sprintf("[%s] Your account confirmation", viper.GetString("email_prefix")), "setup-account-confirm.txt", nil)
				if err != nil {
					log.WithFields(log.Fields{
						"uid":   token.UserName,
						"error": err,
					}).Error("failed to send setup confirmation email to user")
				}

				otpdata, err = QRCode(otptoken)
				if err != nil {
					log.WithFields(log.Fields{
						"uid":   token.UserName,
						"error": err,
					}).Error("failed to render TOTP token as QRCode image")
				}
			}
		}

		vars := map[string]interface{}{
			"token":       nosurf.Token(r),
			"uid":         token.UserName,
			"completed":   completed,
			"questions":   questions,
			"otpRequired": userRec.OTPOnly(),
			"otpdata":     otpdata,
			"otptoken":    otptoken,
			"message":     message}

		ctx.RenderTemplate(w, "setup-account.html", vars)
	})
}

func resetPassword(ctx *app.AppContext, user *ipa.UserRecord, answer *model.SecurityAnswer, token *model.Token, r *http.Request) error {
	challenge := r.FormValue("challenge")
	pass := r.FormValue("password")
	pass2 := r.FormValue("password2")

	if len(pass) < viper.GetInt("min_passwd_len") || len(pass2) < viper.GetInt("min_passwd_len") {
		return errors.New(fmt.Sprintf("Please set a password at least %d characters in length.", viper.GetInt("min_passwd_len")))
	}

	if pass != pass2 {
		return errors.New("Password do not match. Please confirm your password.")
	}

	if user.OTPOnly() && len(challenge) == 0 {
		return errors.New("Please provide a six-digit authentication code")
	}

	if !user.OTPOnly() && viper.GetBool("force_2fa") && answer != nil && !answer.Verify(challenge) {
		return errors.New("The security answer you provided does not match. Please check that you are entering the correct answer.")
	}

	if !user.OTPOnly() {
		// Don't send the security answer as OTP
		challenge = ""
	}

	// Setup password in FreeIPA
	err := setPassword(token.UserName, pass, challenge)
	if err != nil {
		if ierr, ok := err.(*ipa.ErrPasswordPolicy); ok {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": ierr.Error(),
			}).Error("password does not conform to policy")
			return errors.New("Your password is too weak. Please ensure your password includes a number and lower/upper case character")
		}

		if ierr, ok := err.(*ipa.ErrInvalidPassword); ok {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": ierr.Error(),
			}).Error("invalid password from FreeIPA")
			return errors.New("Invalid OTP code.")
		}

		log.WithFields(log.Fields{
			"uid":   token.UserName,
			"error": err.Error(),
		}).Error("failed to set user password in FreeIPA")
		return errors.New("Fatal system error")
	}

	// Destroy token
	err = model.DestroyToken(ctx.DB, token.Token)
	if err != nil {
		log.WithFields(log.Fields{
			"uid":   token.UserName,
			"error": err.Error(),
		}).Error("failed to remove token from database")
		return errors.New("Fatal system error")
	}

	return nil
}

func ResetPasswordHandler(ctx *app.AppContext) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tk, ok := model.VerifyToken(app.ResetSalt, mux.Vars(r)["token"])
		if !ok {
			ctx.RenderNotFound(w)
			return
		}

		token, err := model.FetchToken(ctx.DB, tk, viper.GetInt("reset_max_age"))
		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Failed to fetch token from database")
			ctx.RenderNotFound(w)
			return
		}

		if token.Attempts > viper.GetInt("max_attempts") {
			log.WithFields(log.Fields{
				"token": token.Token,
				"uid":   token.UserName,
			}).Error("Too many attempts for token.")
			ctx.RenderNotFound(w)
			return
		}

		answer, err := model.FetchAnswer(ctx.DB, token.UserName)
		if err != nil && viper.GetBool("require_question_pwreset") {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": err,
			}).Error("Failed to fetch security answer")
			ctx.RenderNotFound(w)
			return
		}

		client := app.NewIpaClient(true)
		userRec, err := client.UserShow(token.UserName)
		if err != nil {
			log.WithFields(log.Fields{
				"uid":   token.UserName,
				"error": err.Error(),
			}).Error("Failed to fetch user record from freeipa")
			ctx.RenderNotFound(w)
			return
		}

		message := ""
		completed := false

		if r.Method == "POST" {
			err := resetPassword(ctx, userRec, answer, token, r)
			if err != nil {
				message = err.Error()
				completed = false

				err := model.IncrementToken(ctx.DB, token.Token)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err.Error(),
					}).Error("Failed to increment token attempts")
				}
			} else {
				completed = true
				err = ctx.SendEmail(token.Email, fmt.Sprintf("[%s] Your password change confirmation", viper.GetString("email_prefix")), "reset-password-confirm.txt", nil)
				if err != nil {
					log.WithFields(log.Fields{
						"uid":   token.UserName,
						"error": err,
					}).Error("failed to send reset confirmation email to user")
				}
			}
		}

		vars := map[string]interface{}{
			"token":            nosurf.Token(r),
			"uid":              token.UserName,
			"completed":        completed,
			"otpRequired":      userRec.OTPOnly(),
			"questionRequired": viper.GetBool("require_question_pwreset"),
			"answer":           answer,
			"message":          message}

		ctx.RenderTemplate(w, "reset-password.html", vars)
	})
}

func forgotPassword(ctx *app.AppContext, r *http.Request) error {
	uid := r.FormValue("uid")
	if len(uid) == 0 {
		return errors.New("Please provide a user name.")
	}

	_, err := model.FetchTokenByUser(ctx.DB, uid, viper.GetInt("setup_max_age"))
	if err == nil {
		log.WithFields(log.Fields{
			"uid": uid,
		}).Error("Forgotpw: user already has active token")
		return nil
	}

	client := app.NewIpaClient(true)
	userRec, err := client.UserShow(uid)
	if err != nil {
		log.WithFields(log.Fields{
			"uid":   uid,
			"error": err,
		}).Error("Forgotpw: invalid uid")
		return nil
	}

	if len(userRec.Email) == 0 {
		log.WithFields(log.Fields{
			"uid": uid,
		}).Error("Forgotpw: missing email address")
		return nil
	}

	_, err = model.FetchAnswer(ctx.DB, uid)
	if err != nil {
		log.WithFields(log.Fields{
			"uid":   uid,
			"error": err,
		}).Error("Forgotpw: Failed to fetch security answer")
		return nil
	}

	token, err := model.CreateToken(ctx.DB, uid, string(userRec.Email))
	if err != nil {
		log.WithFields(log.Fields{
			"uid":   uid,
			"error": err,
		}).Error("Forgotpw: Failed to create token")
		return nil
	}

	vars := map[string]interface{}{
		"link": fmt.Sprintf("%s/auth/resetpw/%s", viper.GetString("email_link_base"), model.SignToken(app.ResetSalt, token.Token))}

	err = ctx.SendEmail(token.Email, fmt.Sprintf("[%s] Please reset your password", viper.GetString("email_prefix")), "reset-password.txt", vars)
	if err != nil {
		log.WithFields(log.Fields{
			"uid":   uid,
			"error": err,
		}).Error("Forgotpw: failed send email to user")
	}

	return nil
}

func ForgotPasswordHandler(ctx *app.AppContext) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := ""
		completed := false

		if r.Method == "POST" {
			err := forgotPassword(ctx, r)
			if err != nil {
				message = err.Error()
				completed = false
			} else {
				completed = true
			}
		}

		vars := map[string]interface{}{
			"token":     nosurf.Token(r),
			"completed": completed,
			"message":   message}

		ctx.RenderTemplate(w, "forgot-password.html", vars)
	})
}

func setPassword(uid, password, otpcode string) error {
	c := app.NewIpaClient(true)

	rand, err := c.ResetPassword(uid)
	if err != nil {
		return err
	}

	err = c.SetPassword(uid, rand, password, otpcode)
	if err != nil {
		return err
	}

	return nil
}

func setTOTP(uid, pass string) (*ipa.OTPToken, error) {
	c := app.NewIpaClient(true)

	_, err := c.Login(uid, pass)
	if err != nil {
		return nil, err
	}

	userRec, err := c.UserShow(uid)
	if err != nil {
		return nil, err
	}

	if userRec.OTPOnly() {
		return c.AddTOTPToken(uid, ipa.AlgorithmSHA1, ipa.DigitsSix, 30)
	}

	return nil, nil
}

func changePassword(ctx *app.AppContext, sid string, user *ipa.UserRecord, answer *model.SecurityAnswer, r *http.Request) error {
	current := r.FormValue("password")
	pass := r.FormValue("new_password")
	pass2 := r.FormValue("new_password2")
	challenge := r.FormValue("challenge")

	if len(current) < viper.GetInt("min_passwd_len") || len(pass) < viper.GetInt("min_passwd_len") || len(pass2) < viper.GetInt("min_passwd_len") {
		return errors.New(fmt.Sprintf("Please set a password at least %d characters in length.", viper.GetInt("min_passwd_len")))
	}

	if pass != pass2 {
		return errors.New("Password do not match. Please confirm your password.")
	}

	if current == pass {
		return errors.New("Current password is the same as new password. Please set a different password.")
	}

	if user.OTPOnly() && len(challenge) == 0 {
		return errors.New("Please provide a six-digit authentication code")
	}

	if !user.OTPOnly() && viper.GetBool("force_2fa") && answer != nil && !answer.Verify(challenge) {
		return errors.New("The security answer you provided does not match. Please check that you are entering the correct answer.")
	}

	if !user.OTPOnly() {
		// Don't send the security answer as OTP
		challenge = ""
	}

	// Change password in FreeIPA
	c := app.NewIpaClient(false)
	c.SetSession(sid)

	err := c.ChangePassword(string(user.Uid), current, pass, challenge)
	if err != nil {
		if ierr, ok := err.(*ipa.IpaError); ok {
			log.WithFields(log.Fields{
				"uid":     user.Uid,
				"message": ierr.Message,
				"code":    ierr.Code,
			}).Error("IPA Error changing password")
			return errors.New(ierr.Message)
		}

		log.WithFields(log.Fields{
			"uid":   user.Uid,
			"error": err.Error(),
		}).Error("failed to set user password in FreeIPA")
		return errors.New("Fatal system error")
	}

	return nil
}

func ChangePasswordHandler(ctx *app.AppContext) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := ctx.GetUser(r)
		if user == nil {
			ctx.RenderError(w, http.StatusInternalServerError)
			return
		}

		session, err := ctx.GetSession(r)
		if err != nil {
			ctx.RenderError(w, http.StatusInternalServerError)
			return
		}

		answer, err := model.FetchAnswer(ctx.DB, string(user.Uid))
		if err != nil && err != sql.ErrNoRows {
			log.WithFields(log.Fields{
				"uid":   string(user.Uid),
				"error": err,
			}).Error("Failed to fetch security question")
			ctx.RenderError(w, http.StatusInternalServerError)
			return
		}

		message := ""
		completed := false

		if r.Method == "POST" {
			sid := session.Values[app.CookieKeySID]
			err := changePassword(ctx, sid.(string), user, answer, r)
			if err != nil {
				message = err.Error()
				completed = false
			} else {
				completed = true
				if len(user.Email) > 0 {
					err = ctx.SendEmail(string(user.Email), fmt.Sprintf("[%s] Your password change confirmation", viper.GetString("email_prefix")), "reset-password-confirm.txt", nil)
					if err != nil {
						log.WithFields(log.Fields{
							"uid":   user.Uid,
							"error": err,
						}).Error("failed to send reset confirmation email to user")
					}
				} else {
					log.WithFields(log.Fields{
						"uid": user.Uid,
					}).Error("changepw: user missing email address")
				}
			}
		}

		vars := map[string]interface{}{
			"token":            nosurf.Token(r),
			"completed":        completed,
			"user":             user,
			"answer":           answer,
			"questionRequired": viper.GetBool("force_2fa"),
			"message":          message}

		ctx.RenderTemplate(w, "change-password.html", vars)
	})
}
