#import <UIKit/UIKit.h>
@interface SwipeViewController : UIViewController
@property(nonatomic,strong) UILabel *result;
@end
@implementation SwipeViewController
- (void)viewDidLoad {
  [super viewDidLoad]; self.view.backgroundColor=UIColor.darkGrayColor;
  self.result=[[UILabel alloc] initWithFrame:CGRectMake(100,100,1500,200)];
  self.result.text=@"Waiting for swipe";self.result.textColor=UIColor.whiteColor;
  self.result.font=[UIFont systemFontOfSize:60];self.result.accessibilityIdentifier=@"swipe-result";
  [self.view addSubview:self.result];
  for(NSNumber *direction in @[@(UISwipeGestureRecognizerDirectionUp),@(UISwipeGestureRecognizerDirectionDown),@(UISwipeGestureRecognizerDirectionLeft),@(UISwipeGestureRecognizerDirectionRight)]) {
    UISwipeGestureRecognizer *recognizer=[[UISwipeGestureRecognizer alloc] initWithTarget:self action:@selector(swiped:)];
    recognizer.direction=direction.unsignedIntegerValue;
    recognizer.allowedTouchTypes=@[@(UITouchTypeIndirect)];
    [self.view addGestureRecognizer:recognizer];
  }
}
- (void)swiped:(UISwipeGestureRecognizer *)gesture {
  NSDictionary *names=@{@(UISwipeGestureRecognizerDirectionUp):@"UP",@(UISwipeGestureRecognizerDirectionDown):@"DOWN",@(UISwipeGestureRecognizerDirectionLeft):@"LEFT",@(UISwipeGestureRecognizerDirectionRight):@"RIGHT"};
  self.result.text=[@"Swipe " stringByAppendingString:names[@(gesture.direction)]];
}
@end
@interface SceneDelegate : UIResponder <UIWindowSceneDelegate>
@property(nonatomic,strong) UIWindow *window;
@end
@implementation SceneDelegate
- (void)scene:(UIScene *)scene willConnectToSession:(UISceneSession *)session options:(UISceneConnectionOptions *)options {
 self.window=[[UIWindow alloc] initWithWindowScene:(UIWindowScene *)scene];
 self.window.rootViewController=[SwipeViewController new];[self.window makeKeyAndVisible];
}
@end
@interface AppDelegate : UIResponder <UIApplicationDelegate>
@end
@implementation AppDelegate
- (UISceneConfiguration *)application:(UIApplication *)application configurationForConnectingSceneSession:(UISceneSession *)session options:(UISceneConnectionOptions *)options {
 UISceneConfiguration *config=[[UISceneConfiguration alloc] initWithName:@"Default Configuration" sessionRole:session.role];
 config.delegateClass=SceneDelegate.class;return config;
}
@end
int main(int argc,char **argv){@autoreleasepool{return UIApplicationMain(argc,argv,nil,NSStringFromClass(AppDelegate.class));}}
